// Package bigolistener implements BIGO's authenticated Linkd notification
// connection. It intentionally contains no capture or emulator integration;
// callers provide the complete session JSON captured by an external tool.
package bigolistener

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const sessionSchemaVersion = 1

// Session contains the stock BIGO account and device identity required by the
// native Linkd login.
type Session struct {
	SchemaVersion         int      `json:"schema_version"`
	CapturedAt            string   `json:"captured_at"`
	UID                   string   `json:"uid"`
	Username              string   `json:"username"`
	CookieBase64          string   `json:"cookie_base64"`
	UUID                  string   `json:"uuid"`
	DeviceID              string   `json:"device_id"`
	CountryCode           string   `json:"country_code"`
	LanguageCode          int16    `json:"language_code"`
	ComposedClientVersion int32    `json:"composed_client_version"`
	SDKVersion            int32    `json:"sdk_version"`
	ClientVersionCode     int16    `json:"client_version_code"`
	ClientType            int8     `json:"client_type"`
	AndroidSDK            int16    `json:"android_sdk"`
	SecurityPacketBase64  string   `json:"security_packet_base64,omitempty"`
	LinkdAddresses        []string `json:"linkd_addresses"`
}

// ParseSessionJSON validates a complete captured BIGO session document.
func ParseSessionJSON(raw string) (Session, error) {
	if strings.TrimSpace(raw) == "" {
		return Session{}, errors.New("BIGO session JSON is empty")
	}
	var session Session
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		return Session{}, fmt.Errorf("parse BIGO session JSON: %w", err)
	}
	if err := session.Validate(); err != nil {
		return Session{}, err
	}
	return session, nil
}

// ValidateSessionJSON validates a captured session without retaining it.
func ValidateSessionJSON(raw string) error {
	_, err := ParseSessionJSON(raw)
	return err
}

// Validate checks all fields needed by the Linkd login.
func (s Session) Validate() error {
	if s.SchemaVersion != sessionSchemaVersion {
		return fmt.Errorf("unsupported BIGO session schema %d", s.SchemaVersion)
	}
	if _, err := s.accountUID(); err != nil {
		return err
	}
	if _, err := s.accountUUID(); err != nil {
		return err
	}
	if strings.TrimSpace(s.Username) == "" || strings.TrimSpace(s.Username) == "0" {
		return errors.New("BIGO session is a guest session; capture a signed-in account")
	}
	if _, err := s.cookie(); err != nil {
		return err
	}
	if _, err := s.securityPacket(); err != nil {
		return err
	}
	if strings.TrimSpace(s.DeviceID) == "" {
		return errors.New("BIGO session device id is empty")
	}
	if strings.TrimSpace(s.CountryCode) == "" {
		return errors.New("BIGO session country code is empty")
	}
	if s.SDKVersion == 0 || s.ClientVersionCode == 0 || s.ClientType == 0 || s.AndroidSDK == 0 {
		return errors.New("BIGO session client identity is incomplete")
	}
	validAddresses := 0
	for _, value := range s.LinkdAddresses {
		if _, err := parseLinkdAddress(value); err == nil {
			validAddresses++
		}
	}
	if validAddresses == 0 {
		return errors.New("BIGO session does not contain a valid Linkd address")
	}
	return nil
}

func (s Session) accountUID() (uint32, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(s.UID), 10, 32)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("invalid BIGO account uid %q", s.UID)
	}
	return uint32(value), nil
}

func (s Session) accountUUID() (uint32, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(s.UUID), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid BIGO account uuid %q", s.UUID)
	}
	return uint32(value), nil
}

func decodeBase64(value, emptyMessage, invalidMessage string, allowEmpty bool) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		if allowEmpty {
			return nil, nil
		}
		return nil, errors.New(emptyMessage)
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil || (!allowEmpty && len(decoded) == 0) {
		return nil, errors.New(invalidMessage)
	}
	return decoded, nil
}

func (s Session) cookie() ([]byte, error) {
	return decodeBase64(s.CookieBase64, "BIGO session cookie is empty", "BIGO session cookie is not valid base64", false)
}

func (s Session) securityPacket() ([]byte, error) {
	return decodeBase64(s.SecurityPacketBase64, "", "BIGO security packet is not valid base64", true)
}

func parseLinkdAddress(value string) (string, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "/")
	if value == "" {
		return "", errors.New("empty Linkd address")
	}
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
