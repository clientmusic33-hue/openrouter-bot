package research

import (
	"context"
	"strings"
	"testing"
)

func TestExtractTextFromHTMLStripsScriptsAndTags(t *testing.T) {
	raw := `<html><head><title>Quantum Computing</title><script>alert("xss")</script><style>body{color:red}</style></head>
	<body><h1>Overview</h1><p>Qubits use superposition &amp; entanglement.</p></body></html>`

	title, text := ExtractTextFromHTML(raw, 500)
	if title != "Quantum Computing" {
		t.Errorf("title = %q, want Quantum Computing", title)
	}
	if strings.Contains(text, "alert") || strings.Contains(text, "color:red") {
		t.Errorf("script/style leaked into extracted text: %q", text)
	}
	if !strings.Contains(text, "superposition & entanglement") {
		t.Errorf("expected unescaped body text, got %q", text)
	}
}

func TestDeduplicateSources(t *testing.T) {
	sources := []Source{
		{Title: "A", URL: "https://example.com/post/#section1", Snippet: "Unique content one"},
		{Title: "A dup URL", URL: "https://example.com/post", Snippet: "Different snippet"},
		{Title: "B dup content", URL: "https://other.org/post", Snippet: "Unique content one"},
		{Title: "C distinct", URL: "https://third.org/page", Snippet: "Completely distinct content"},
	}

	deduped := DeduplicateSources(sources)
	if len(deduped) != 2 {
		t.Fatalf("DeduplicateSources returned %d sources, want 2: %+v", len(deduped), deduped)
	}
}

func TestFetchPageBlocksSSRF(t *testing.T) {
	_, err := FetchPage(context.Background(), nil, "http://127.0.0.1:8080/admin", 500)
	if err == nil {
		t.Fatal("FetchPage must block localhost SSRF")
	}
	_, err = FetchPage(context.Background(), nil, "http://169.254.169.254/latest/meta-data", 500)
	if err == nil {
		t.Fatal("FetchPage must block cloud metadata SSRF")
	}
}
