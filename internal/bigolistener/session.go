// Package bigolistener implements BIGO's authenticated native Linkd connection.
// Messaging credentials are issued from the user's web cookie on connection;
// no Android capture, browser process, or second saved account is required.
package bigolistener

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Session is an ephemeral messaging ticket, not a persisted account document.
type Session struct {
	UID            uint32
	Username       string
	Cookie         []byte
	DeviceID       string
	LinkdAddresses []string
}

// LinkdAddresses prefers provider-advertised routes and retains known native
// routes as fallback. The current web bootstrap often returns an empty list.
// These are routing hints, not captured account/session credentials.
func LinkdAddresses(advertised []string) []string {
	candidates := append(append([]string(nil), advertised...),
		"164.90.67.6:54953", "164.90.64.60:1299")
	result := make([]string, 0, len(candidates))
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if address, err := parseLinkdAddress(candidate); err == nil && !seen[address] {
			seen[address] = true
			result = append(result, address)
		}
	}
	return result
}

func (s Session) Validate() error {
	if s.UID == 0 || strings.TrimSpace(s.Username) == "" {
		return errors.New("BIGO did not issue a real-account messaging identity; replace the web cookie")
	}
	if len(s.Cookie) == 0 || len(s.Cookie) > 65535 {
		return errors.New("BIGO messaging ticket is empty or too large")
	}
	if strings.TrimSpace(s.DeviceID) == "" || len(s.DeviceID) > 65535 || len(s.Username) > 65535 {
		return errors.New("BIGO messaging identity is incomplete or too large")
	}
	for _, value := range s.LinkdAddresses {
		if _, err := parseLinkdAddress(value); err == nil {
			return nil
		}
	}
	return errors.New("BIGO messaging identity has no valid Linkd address")
}

func parseLinkdAddress(value string) (string, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "/")
	host, portText, err := net.SplitHostPort(value)
	if err != nil || strings.TrimSpace(host) == "" {
		return "", fmt.Errorf("invalid Linkd address %q", value)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid Linkd port in %q", value)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
