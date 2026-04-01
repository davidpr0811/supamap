# supamap

Supamap is a simple tool to find all supabase tables from a website and save them it can help you to scan your site and find potetion securety mistakes in rls

Usage 
go run supamap.go --url targeturl --out output_folder

once you run the program it will automaticly find the supabase default creds on the target url by scanning all js files and then it will look into the shema file to find all tables after that it starts to extract them and store them in human readable json files on your output folder 

if you find improvements for this project just open a pull request or ask in issues

Disclaimer this tool is only for testing on your own site dont use it on sites that not belong to you
