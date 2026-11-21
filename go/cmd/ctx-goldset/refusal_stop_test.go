package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/goldset"
)

// #459 point 2: a refused redirect is the configuration of the -backend row,
// met by every call alike. Scoring it per block and going on wrote a slice
// with every block discarded; the generation stops on the first refusal and
// returns it, as ctx-goldbench and ctx-armsweep end their runs.

func redirectingBackend(t *testing.T) (*goldset.ChatClient, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var homeSeen, otherSeen atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherSeen.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"What does the note say?"}}]}`)
	}))
	t.Cleanup(other.Close)
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		homeSeen.Add(1)
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(home.Close)
	return &goldset.ChatClient{URL: home.URL + "/v1/chat/completions", Model: "m", HTTP: &http.Client{}}, &homeSeen, &otherSeen
}

func TestGenerationStopsOnARefusedRedirect(t *testing.T) {
	const n, conc = 40, 2
	t.Run("g-q", func(t *testing.T) {
		client, homeSeen, otherSeen := redirectingBackend(t)
		blocks := make([]goldset.Block, n)
		for i := range blocks {
			blocks[i] = goldset.Block{ID: fmt.Sprintf("%08d-block", i), Title: "t", Content: "private content"}
		}
		cases, _, _, err := generateQuestions(context.Background(), client, blocks, conc)
		if err == nil || len(cases) != 0 {
			t.Fatalf("generateQuestions = %d cases, err %v; want the refusal and no cases", len(cases), err)
		}
		if h := homeSeen.Load(); h > conc {
			t.Errorf("the backend saw %d calls after the refusal, want at most %d (one per worker)", h, conc)
		}
		if o := otherSeen.Load(); o != 0 {
			t.Errorf("the other origin saw %d calls", o)
		}
	})
	t.Run("slices", func(t *testing.T) {
		client, homeSeen, otherSeen := redirectingBackend(t)
		jobs := make([]job, n)
		for i := range jobs {
			jobs[i] = job{prompt: "private prompt", proto: goldset.Case{Slice: goldset.SliceMH}}
		}
		cases, _, _, _, err := runGeneration(context.Background(), client, goldset.SliceMH, jobs, conc, n)
		if err == nil || len(cases) != 0 {
			t.Fatalf("runGeneration = %d cases, err %v; want the refusal and no cases", len(cases), err)
		}
		if h := homeSeen.Load(); h > conc {
			t.Errorf("the backend saw %d calls after the refusal, want at most %d (one per worker)", h, conc)
		}
		if o := otherSeen.Load(); o != 0 {
			t.Errorf("the other origin saw %d calls", o)
		}
	})
}
