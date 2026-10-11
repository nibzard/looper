package main

import (
	"github.com/nibzard/looper/internal/looper"
	"os"
)

func main() { os.Exit(looper.Main(os.Args[1:], os.Stdout, os.Stderr)) }
