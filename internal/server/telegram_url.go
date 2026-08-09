package server

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

const defaultTelegramBaseURL = "https://api.telegram.org"

// telegramAPIURL accepts either a Telegram API base URL or a printf-style
// endpoint template whose two %s placeholders are the bot token and method.
func telegramAPIURL(baseURL, token, method string) (*url.URL, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		raw = defaultTelegramBaseURL
	}

	placeholderCount := strings.Count(raw, "%s")
	if placeholderCount != 0 && placeholderCount != 2 {
		return nil, errors.New("Telegram API URL must contain either no %s placeholders or exactly two")
	}
	if placeholderCount == 2 {
		if telegramPlaceholderInAuthority(raw) {
			return nil, errors.New("Telegram API URL placeholders are not allowed in the host")
		}
		endpoint := strings.Replace(raw, "%s", token, 1)
		endpoint = strings.Replace(endpoint, "%s", method, 1)
		return parseOutboundURL(endpoint, true)
	}

	parsed, err := parseOutboundURL(raw, true)
	if err != nil {
		return nil, err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/bot" + token + "/" + method
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

func validateTelegramAPIURL(ctx context.Context, baseURL, token, method string) (*url.URL, error) {
	parsed, err := telegramAPIURL(baseURL, token, method)
	if err != nil {
		return nil, err
	}
	if _, err := resolvePublicAddresses(ctx, parsed.Hostname()); err != nil {
		return nil, err
	}
	return parsed, nil
}

func telegramPlaceholderInAuthority(raw string) bool {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return false
	}
	authority := raw[schemeEnd+3:]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	return strings.Contains(authority, "%s")
}
