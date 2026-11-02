package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var blockedWebhookPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func encryptWebhookSecret(key []byte, secret string) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("webhook encryption is not configured")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("invalid webhook encryption key")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("could not initialize webhook encryption")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("could not create webhook secret nonce")
	}
	return aead.Seal(nonce, nonce, []byte(secret), nil), nil
}

func decryptWebhookSecret(key, encrypted []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("webhook encryption is not configured")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("invalid webhook encryption key")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(encrypted) < aead.NonceSize() {
		return nil, errors.New("stored webhook secret is invalid")
	}
	nonce, ciphertext := encrypted[:aead.NonceSize()], encrypted[aead.NonceSize():]
	secret, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, errors.New("could not decrypt stored webhook secret")
	}
	return secret, nil
}

func validateWebhookURL(raw string) (string, error) {
	if len(raw) > 2048 {
		return "", errors.New("webhook URL must be 2048 characters or fewer")
	}
	u, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("webhook URL must be an HTTPS URL without credentials or a fragment")
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", errors.New("webhook URL must use port 443")
	}
	if parsedIP := net.ParseIP(u.Hostname()); parsedIP != nil && !isPublicWebhookIP(parsedIP) {
		return "", errors.New("webhook URL must resolve to a public address")
	}
	return u.String(), nil
}

func isPublicWebhookIP(ip net.IP) bool {
	addr, err := netip.ParseAddr(ip.String())
	if err != nil {
		return false
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsMulticast() {
		return false
	}
	for _, prefix := range blockedWebhookPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func safeWebhookDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return nil, errors.New("webhook connection must use HTTPS port 443")
	}
	var ips []net.IP
	if parsedIP := net.ParseIP(host); parsedIP != nil {
		ips = []net.IP{parsedIP}
	} else {
		resolved, resolveErr := net.DefaultResolver.LookupIPAddr(ctx, host)
		if resolveErr != nil {
			return nil, errors.New("webhook host could not be resolved")
		}
		for _, item := range resolved {
			ips = append(ips, item.IP)
		}
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	for _, ip := range ips {
		if !isPublicWebhookIP(ip) {
			continue
		}
		connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), "443"))
		if dialErr == nil {
			return connection, nil
		}
	}
	return nil, errors.New("webhook host has no reachable public address")
}

func newWebhookHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           safeWebhookDialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConnsPerHost:   2,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func deliverWebhook(ctx context.Context, client *http.Client, delivery WebhookDelivery, secret []byte) (int, bool, string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.TargetURL, bytes.NewReader(delivery.Payload))
	if err != nil {
		return 0, false, "could not create webhook request"
	}
	timestamp := time.Now().Unix()
	mac := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(mac, "%d.", timestamp)
	_, _ = mac.Write(delivery.Payload)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "ModelRail-Webhooks/1.0")
	request.Header.Set("X-ModelRail-Delivery", delivery.ID)
	request.Header.Set("X-ModelRail-Event", delivery.Event)
	request.Header.Set("X-ModelRail-Signature", "t="+strconv.FormatInt(timestamp, 10)+",v1="+hex.EncodeToString(mac.Sum(nil)))

	response, err := client.Do(request)
	if err != nil {
		return 0, true, "webhook request failed"
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 4096)
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response.StatusCode, false, ""
	}
	message := "webhook endpoint returned HTTP " + strconv.Itoa(response.StatusCode)
	retryable := response.StatusCode == http.StatusRequestTimeout ||
		response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError
	return response.StatusCode, retryable, message
}

func runWebhookDeliveryWorker(ctx context.Context, store WebhookStore, encryptionKey []byte) {
	client := newWebhookHTTPClient()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer client.CloseIdleConnections()

	for {
		if ctx.Err() != nil {
			return
		}
		claimCtx, cancelClaim := context.WithTimeout(ctx, 5*time.Second)
		delivery, err := store.ClaimWebhookDelivery(claimCtx)
		cancelClaim()
		if errors.Is(err, ErrNoWebhookDelivery) {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("webhook worker could not claim delivery", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			continue
		}

		secret, decryptErr := decryptWebhookSecret(encryptionKey, delivery.SecretCiphertext)
		statusCode, retryable, message := 0, false, ""
		if decryptErr != nil {
			message = "webhook signing secret could not be decrypted"
		} else {
			deliveryCtx, cancelDelivery := context.WithTimeout(ctx, 20*time.Second)
			statusCode, retryable, message = deliverWebhook(deliveryCtx, client, delivery, secret)
			cancelDelivery()
		}

		finishCtx, cancelFinish := context.WithTimeout(context.Background(), 5*time.Second)
		if message == "" {
			if err := store.CompleteWebhookDelivery(finishCtx, delivery.ID, delivery.LeaseToken, statusCode); err != nil {
				slog.Error("webhook delivery completion failed", "delivery_id", delivery.ID, "error", err)
			} else {
				slog.Info("webhook delivered", "delivery_id", delivery.ID, "event", delivery.Event, "attempt", delivery.AttemptCount, "status_code", statusCode)
			}
		} else {
			state, err := store.FailWebhookDelivery(finishCtx, delivery.ID, delivery.LeaseToken, message, statusCode, retryable)
			if err != nil {
				slog.Error("webhook delivery failure could not be recorded", "delivery_id", delivery.ID, "error", err)
			} else {
				slog.Warn("webhook delivery attempt failed", "delivery_id", delivery.ID, "event", delivery.Event, "attempt", delivery.AttemptCount, "status_code", statusCode, "delivery_state", state, "error", message)
			}
		}
		cancelFinish()
	}
}
