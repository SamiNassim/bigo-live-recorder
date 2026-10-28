package bigo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// MessagingCredentials is an in-memory Linkd ticket issued from the web cookie.
// It is refreshed on reconnect, never saved as another account credential.
type MessagingCredentials struct {
	UID       uint32
	Username  string
	Cookie    []byte
	DeviceID  string
	Addresses []string
}

// Messaging obtains the real account's ticket through the same rate-limited
// HTTP client and browser identity as playback. Guest credentials are never
// accepted as an authenticated listener identity.
func (c *Client) Messaging(ctx context.Context, deviceID string) (MessagingCredentials, error) {
	var response struct {
		Code int `json:"code"`
		Data struct {
			RealUser *struct {
				UID      json.RawMessage `json:"userId"`
				Username string          `json:"userName"`
				Token    string          `json:"uidToken"`
				DeviceID string          `json:"deviceId"`
			} `json:"realUser"`
			Addresses []string `json:"wssLinkd"`
		} `json:"data"`
	}
	if deviceID == "" {
		return MessagingCredentials{}, fmt.Errorf("BIGO listener device identity is empty")
	}
	if err := c.request(ctx, http.MethodPost, "/studio/getWebSocketLink", url.Values{"deviceId": {deviceID}}, &response); err != nil {
		return MessagingCredentials{}, err
	}
	user := response.Data.RealUser
	// Only realUser contains an authenticated account ticket. BIGO can return
	// "0" as that account's protocol username; it is not a guest marker.
	if response.Code != 0 || user == nil || strings.TrimSpace(user.Username) == "" {
		return MessagingCredentials{}, ErrLoginRequired
	}
	var uidText string
	if err := json.Unmarshal(user.UID, &uidText); err != nil {
		uidText = string(user.UID)
	}
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil || uid == 0 || user.DeviceID == "" {
		return MessagingCredentials{}, fmt.Errorf("BIGO returned an incomplete real-account messaging identity")
	}
	token := strings.TrimSuffix(strings.TrimSpace(user.Token), "###VER2")
	cookie, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		cookie, err = base64.RawStdEncoding.DecodeString(token)
	}
	if err != nil || len(cookie) == 0 || len(cookie) > 65535 {
		return MessagingCredentials{}, fmt.Errorf("BIGO returned an invalid messaging ticket")
	}
	return MessagingCredentials{UID: uint32(uid), Username: user.Username, Cookie: cookie,
		DeviceID: user.DeviceID, Addresses: response.Data.Addresses}, nil
}
