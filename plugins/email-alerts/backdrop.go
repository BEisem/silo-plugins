package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Show backdrops behind the show header are CSS backgrounds, and Gmail
// ignores cid: images in CSS. So the email links to a public, signed plugin
// route that serves the backdrop as a JPEG, already darkened with the card
// colour so the text over it stays readable in clients without gradients.

const (
	keyImageSecret = "image-secret"
	bgCacheSize    = 64
	bgCacheTTL     = 6 * time.Hour
)

type bgCache struct {
	mu      sync.Mutex
	secret  []byte
	entries map[string]bgEntry
}

type bgEntry struct {
	data []byte
	at   time.Time
}

func (a *App) imageSecret(ctx context.Context) ([]byte, error) {
	a.bg.mu.Lock()
	defer a.bg.mu.Unlock()
	if a.bg.secret != nil {
		return a.bg.secret, nil
	}
	var hexSecret string
	found, err := a.store.read(ctx, keyImageSecret, &hexSecret)
	if err != nil {
		return nil, err
	}
	if !found || hexSecret == "" {
		hexSecret = newToken()
		if err := a.store.write(ctx, keyImageSecret, hexSecret); err != nil {
			return nil, err
		}
	}
	a.bg.secret = []byte(hexSecret)
	return a.bg.secret, nil
}

func bgSignature(secret []byte, series string, dim int, tint string) string {
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "%s|%d|%s", series, dim, tint)
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// backdropBackgroundURL returns the public URL of a series' or movie's
// darkened backdrop, or "" when the feature is off or can't be served.
func (a *App) backdropBackgroundURL(ctx context.Context, env RenderEnv, seriesID string) string {
	t := env.Settings.Theme
	if !t.BackdropBehindShow || seriesID == "" || env.PluginBase == "" {
		return ""
	}
	secret, err := a.imageSecret(ctx)
	if err != nil {
		return ""
	}
	tint := strings.TrimPrefix(strings.ToLower(t.CardColor), "#")
	if _, ok := parseHex(tint); !ok {
		tint = "181b22"
	}
	dim := t.BackdropDim
	q := url.Values{}
	q.Set("s", seriesID)
	q.Set("d", strconv.Itoa(dim))
	q.Set("c", tint)
	q.Set("sig", bgSignature(secret, seriesID, dim, tint))
	return env.PluginBase + "/public/bg?" + q.Encode()
}

// serveBackdrop answers /public/bg.
func (a *App) serveBackdrop(ctx context.Context, req request) *pluginv1.HandleHTTPResponse {
	series, tint, sig := req.q("s"), req.q("c"), req.q("sig")
	dim, err := strconv.Atoi(req.q("d"))
	if series == "" || sig == "" || err != nil || dim < 0 || dim > 95 {
		return imageError(400)
	}
	secret, err := a.imageSecret(ctx)
	if err != nil {
		return imageError(503)
	}
	if !hmac.Equal([]byte(sig), []byte(bgSignature(secret, series, dim, tint))) {
		return imageError(403)
	}
	key := series + "|" + strconv.Itoa(dim) + "|" + tint
	a.bg.mu.Lock()
	if e, ok := a.bg.entries[key]; ok && time.Since(e.at) < bgCacheTTL {
		a.bg.mu.Unlock()
		return imageResponse(e.data)
	}
	a.bg.mu.Unlock()

	ep := a.endpoints(ctx)
	client := a.client(ctx, ep)
	d := a.detail(ctx, client, series)
	if d == nil || d.BackdropURL == "" {
		return imageError(404)
	}
	raw, _, err := client.Image(ctx, d.BackdropURL)
	if err != nil {
		// The cached detail's presigned URL may have expired; try a fresh one.
		if d = a.freshDetail(ctx, client, series); d == nil || d.BackdropURL == "" {
			return imageError(404)
		}
		if raw, _, err = client.Image(ctx, d.BackdropURL); err != nil {
			a.log.Warn("show backdrop download failed", "series", series, "url", redactURL(d.BackdropURL), "error", err)
			return imageError(502)
		}
	}
	tintColor, _ := parseHex(tint)
	data, err := darken(raw, tintColor, dim)
	if err != nil {
		return imageError(502)
	}
	a.bg.mu.Lock()
	if a.bg.entries == nil || len(a.bg.entries) >= bgCacheSize {
		a.bg.entries = map[string]bgEntry{}
	}
	a.bg.entries[key] = bgEntry{data: data, at: time.Now()}
	a.bg.mu.Unlock()
	return imageResponse(data)
}

// darken lays tint over img at dim percent opacity and encodes a JPEG.
func darken(raw []byte, tint color.NRGBA, dim int) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		return nil, errors.New("empty image")
	}
	out := image.NewRGBA(b)
	draw.Draw(out, b, src, b.Min, draw.Src)
	tint.A = uint8(dim * 255 / 100)
	draw.Draw(out, b, image.NewUniform(tint), image.Point{}, draw.Over)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseHex(s string) (color.NRGBA, bool) {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return color.NRGBA{}, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.NRGBA{}, false
	}
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}, true
}

func imageResponse(data []byte) *pluginv1.HandleHTTPResponse {
	return &pluginv1.HandleHTTPResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type":  "image/jpeg",
			"Cache-Control": "public, max-age=604800",
		},
		Body: data,
	}
}

func imageError(status int) *pluginv1.HandleHTTPResponse {
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers:    map[string]string{"Content-Type": "text/plain", "Cache-Control": "no-store"},
		Body:       []byte(strconv.Itoa(status)),
	}
}
