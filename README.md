go-bulk-downloader

Instructions for building windows executable

1. In addition to go you will need mingw-w64
run - sudo apt install mingw-w64

2. Set the C compiler to a cross compiler for windows 
run - export CC=/usr/bin/x86_64-w64-mingw32-gcc-win32

3. To compile run this command. It enables the C cross compiler flags OS for windows and disables command line

run - env GOOS=windows CGO_ENABLED=1 go build -ldflags -H=windowsgui