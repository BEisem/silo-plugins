package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"
	"time"
)

func TestDarkenBlendsTowardsTint(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range src.Pix {
		src.Pix[i] = 255 // white
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, nil); err != nil {
		t.Fatal(err)
	}
	out, err := darken(buf.Bytes(), color.NRGBA{R: 0, G: 0, B: 0}, 72)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	r, _, _, _ := img.At(1, 1).RGBA()
	// 28% of white is about 71/255.
	if v := r >> 8; v < 60 || v > 85 {
		t.Fatalf("darkened red = %d, want about 71", v)
	}
}

func TestBackdropSignature(t *testing.T) {
	secret := []byte("s3cret")
	sig := bgSignature(secret, "series-1", 72, "181b22")
	if sig != bgSignature(secret, "series-1", 72, "181b22") {
		t.Fatal("signature not stable")
	}
	for _, other := range []string{
		bgSignature(secret, "series-2", 72, "181b22"),
		bgSignature(secret, "series-1", 10, "181b22"),
		bgSignature([]byte("other"), "series-1", 72, "181b22"),
	} {
		if other == sig {
			t.Fatal("different input produced the same signature")
		}
	}
}

func TestShowBackdropBackgroundInTemplate(t *testing.T) {
	env := RenderEnv{Settings: defaultSettings(), ServerName: "Silo", Mode: "instant", Now: time.Now(),
		BackdropBG: func(id string) string { return "https://silo.example.com/bg?s=" + id }}
	entries := sampleEntries()[:1]
	entries[0].Series.BackdropURL = "/art/backdrop.webp"
	r, err := Render(context.Background(), env, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.HTML, `background-image:url('https://silo.example.com/bg?s=series:sample-harbour-lights')`) {
		t.Fatal("show header has no backdrop background")
	}
	if !strings.Contains(r.HTML, `background="https://silo.example.com/bg?s=series:sample-harbour-lights"`) {
		t.Fatal("show header has no background attribute fallback")
	}

	// No backdrop on the series: plain card colour, no background URL.
	entries[0].Series.BackdropURL = ""
	r, err = Render(context.Background(), env, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.HTML, "background-image:url(") {
		t.Fatal("background used for a series without a backdrop")
	}
}

func TestMovieBackdropBackgroundInTemplate(t *testing.T) {
	env := RenderEnv{Settings: defaultSettings(), ServerName: "Silo", Mode: "instant", Now: time.Now(),
		BackdropBG: func(id string) string { return "https://silo.example.com/bg?s=" + id }}
	entries := sampleEntries()[2:]
	entries[0].Card.BackdropURL = "/art/movie-backdrop.webp"
	r, err := Render(context.Background(), env, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`background-image:url('https://silo.example.com/bg?s=movie:sample-3')`,
		`background="https://silo.example.com/bg?s=movie:sample-3"`,
	} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("movie card missing %s", want)
		}
	}

	// No backdrop on the movie: plain card colour.
	entries[0].Card.BackdropURL = ""
	r, err = Render(context.Background(), env, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.HTML, "background-image:url(") {
		t.Fatal("background used for a movie without a backdrop")
	}
}
