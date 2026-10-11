package integlife

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const defaultAPIURL = "https://api.integ.life"

type Config struct {
	APIURL      string
	APIToken    string
	DBPath      string
	BridgeURL   string
	BridgeToken string
}

func LoadConfig() Config {
	return Config{
		APIURL:      firstNonEmpty(os.Getenv("INTEGLIFE_API_URL"), defaultAPIURL),
		APIToken:    firstNonEmpty(os.Getenv("INTEGLIFE_API_TOKEN"), loadAPIToken()),
		DBPath:      firstNonEmpty(os.Getenv("INTEGLIFE_DB_PATH"), defaultDBPath()),
		BridgeURL:   os.Getenv("LIFE_BRIDGE_URL"),
		BridgeToken: os.Getenv("LIFE_BRIDGE_TOKEN"),
	}
}

func loadAPIToken() string {
	path := tokenFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func SaveAPIToken(token string) error {
	path := tokenFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0600)
}

func DeleteAPIToken() error {
	path := tokenFilePath()
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func tokenFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".integlife/token"
	}
	return filepath.Join(home, ".integlife", "token")
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".integlife/integlife.db"
	}
	return filepath.Join(home, ".integlife", "integlife.db")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
