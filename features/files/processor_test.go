package files

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestProcessCSVComputesStats(t *testing.T) {
	csvData := "name,score\nAlice,90\nBob,80\nCharlie,100\n"
	ext, err := Process("scores.csv", "text/csv", strings.NewReader(csvData), DefaultMaxFileSize)
	if err != nil {
		t.Fatalf("Process CSV: %v", err)
	}
	if ext.Kind != KindCSV {
		t.Errorf("Kind = %s, want csv", ext.Kind)
	}
	if !strings.Contains(ext.Content, "avg=90") || !strings.Contains(ext.Content, "min=80") {
		t.Errorf("CSV stats missing from content:\n%s", ext.Content)
	}
}

func TestProcessJSONAndSizeCap(t *testing.T) {
	jsonData := `{"service":"bot","replicas":2}`
	ext, err := Process("config.json", "application/json", strings.NewReader(jsonData), 1024)
	if err != nil {
		t.Fatalf("Process JSON: %v", err)
	}
	if ext.Kind != KindJSON || !strings.Contains(ext.Summary, "top-level keys") {
		t.Errorf("unexpected JSON summary: %+v", ext)
	}

	// Oversized file must be rejected.
	if _, err := Process("huge.txt", "text/plain", strings.NewReader(strings.Repeat("x", 2048)), 100); err == nil {
		t.Fatal("expected size limit error")
	}
}

func TestProcessDOCXAndPDF(t *testing.T) {
	// Build a minimal valid DOCX in memory.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	_, _ = w.Write([]byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Hello from DOCX</w:t></w:r></w:p></w:body></w:document>`))
	_ = zw.Close()

	docxOut, err := Process("report.docx", "", bytes.NewReader(buf.Bytes()), DefaultMaxFileSize)
	if err != nil {
		t.Fatalf("Process DOCX: %v", err)
	}
	if !strings.Contains(docxOut.Content, "Hello from DOCX") {
		t.Errorf("DOCX content = %q", docxOut.Content)
	}

	// Minimal valid PDF with literal text stream.
	pdfBytes := []byte("%PDF-1.4\n1 0 obj\n<< /Length 44 >>\nstream\nBT /F1 12 Tf 72 712 Td (Hello from PDF) Tj ET\nendstream\nendobj\n%%EOF")
	pdfOut, err := Process("doc.pdf", "application/pdf", bytes.NewReader(pdfBytes), DefaultMaxFileSize)
	if err != nil {
		t.Fatalf("Process PDF: %v", err)
	}
	if !strings.Contains(pdfOut.Content, "Hello from PDF") {
		t.Errorf("PDF content = %q", pdfOut.Content)
	}
}
