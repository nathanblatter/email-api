package service

import (
	"crypto/rand"
	"encoding/hex"
	"mime"
)

type mimeDecoder = mime.WordDecoder

func rawID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
