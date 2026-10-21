package core

import (
	"net/url"
	"strconv"
	"strings"
)

func redisOptions(raw string) (addr, password string, db int) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw, "", 0
	}
	addr = parsed.Host
	if parsed.User != nil {
		if pass, ok := parsed.User.Password(); ok {
			password = pass
		}
	}
	if path := strings.Trim(parsed.Path, "/"); path != "" {
		if value, err := strconv.Atoi(path); err == nil {
			db = value
		}
	}
	return addr, password, db
}

// RedisAddr exposes the parsed broker address for other packages.
func RedisAddr(raw string) string { addr, _, _ := redisOptions(raw); return addr }

// RedisPassword exposes the parsed broker password for other packages.
func RedisPassword(raw string) string { _, password, _ := redisOptions(raw); return password }

// RedisDB exposes the parsed broker database index for other packages.
func RedisDB(raw string) int { _, _, db := redisOptions(raw); return db }
