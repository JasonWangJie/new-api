package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
)

// Keys are configured as id:base64-32-byte-key pairs separated by commas.
// Keep previous keys configured until all requests using them reach a terminal state.
func ImageEncryptionAvailable() bool {
	_, _, err := imagePayloadKeys()
	return err == nil
}

func imagePayloadKeys() (map[string][]byte, string, error) {
	keys := make(map[string][]byte)
	active := strings.TrimSpace(os.Getenv("ASYNC_IMAGE_ACTIVE_KEY_ID"))
	for entry := range strings.SplitSeq(os.Getenv("ASYNC_IMAGE_PAYLOAD_KEYS"), ",") {
		id, value, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || id == "" || len(id) > 32 || strings.ContainsAny(id, " \r\n\t") {
			return nil, "", errors.New("image encryption keys are not configured correctly")
		}
		key, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil || len(key) != 32 {
			return nil, "", errors.New("image encryption requires a 32-byte key")
		}
		if _, exists := keys[id]; exists {
			return nil, "", errors.New("duplicate image encryption key id")
		}
		keys[id] = key
		if active == "" {
			active = id
		}
	}
	if keys[active] == nil {
		return nil, "", errors.New("active image encryption key is unavailable")
	}
	return keys, active, nil
}

func EncryptImagePayload(data []byte, binding string) ([]byte, error) {
	keys, active, err := imagePayloadKeys()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(keys[active])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := append([]byte{byte(len(active))}, []byte(active)...)
	header = append(header, nonce...)
	return gcm.Seal(header, nonce, data, []byte("new-api:images:v1:"+binding)), nil
}

func DecryptImagePayload(data []byte, binding string) ([]byte, error) {
	keys, _, err := imagePayloadKeys()
	if err != nil {
		return nil, err
	}
	if len(data) < 2 {
		return nil, errors.New("invalid encrypted image payload")
	}
	keyLength := int(data[0])
	if keyLength == 0 || len(data) <= keyLength+1 {
		return nil, errors.New("invalid encrypted image payload")
	}
	key := keys[string(data[1:keyLength+1])]
	if key == nil {
		return nil, errors.New("image payload encryption key is unavailable")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	start := keyLength + 1
	if len(data) < start+gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("invalid encrypted image payload")
	}
	return gcm.Open(nil, data[start:start+gcm.NonceSize()], data[start+gcm.NonceSize():], []byte("new-api:images:v1:"+binding))
}
