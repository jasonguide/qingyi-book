package main

// 极小的通用垫片，避免在多个文件里重复 import。

import (
	"encoding/json"
	"io"
	"os"
)

func getenv(key string) string { return os.Getenv(key) }

func jsonNewDecoder(r io.Reader) *json.Decoder { return json.NewDecoder(r) }
