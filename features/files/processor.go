// Package files implements bounded, modular file intelligence for TXT, CSV,
// JSON, DOCX, PDF, and source code files.
package files

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"openrouter-bot/internal/security"
)

// DefaultMaxFileSize is the default upper bound on uploaded files (10 MiB).
const DefaultMaxFileSize int64 = 10 << 20

// maxPromptContentChars caps how much extracted file text is injected into a
// single model prompt.
const maxPromptContentChars = 12000

// Kind identifies the category of an uploaded file.
type Kind string

const (
	KindText     Kind = "text"
	KindCode     Kind = "code"
	KindCSV      Kind = "csv"
	KindJSON     Kind = "json"
	KindDOCX     Kind = "docx"
	KindPDF      Kind = "pdf"
	KindImage    Kind = "image"
	KindUnknown  Kind = "unknown"
)

// ExtractedFile holds the structured representation of an uploaded file.
type ExtractedFile struct {
	Filename string
	Kind     Kind
	Language string
	Size     int64
	Summary  string
	Content  string
}

var codeExtensions = map[string]string{
	".go":         "Go",
	".py":         "Python",
	".js":         "JavaScript",
	".ts":         "TypeScript",
	".tsx":        "TypeScript (React)",
	".jsx":        "JavaScript (React)",
	".rs":         "Rust",
	".java":       "Java",
	".c":          "C",
	".cpp":        "C++",
	".cc":         "C++",
	".h":          "C/C++ Header",
	".cs":         "C#",
	".rb":         "Ruby",
	".php":        "PHP",
	".sh":         "Shell",
	".bash":       "Bash",
	".sql":        "SQL",
	".yaml":       "YAML",
	".yml":        "YAML",
	".toml":       "TOML",
	".dockerfile": "Dockerfile",
	".kt":         "Kotlin",
	".swift":      "Swift",
}

// DetectKind classifies a filename and MIME type.
func DetectKind(filename, mimeType string) (Kind, string) {
	clean := security.SanitizeFilename(filename)
	lower := strings.ToLower(clean)
	ext := strings.ToLower(filepath.Ext(lower))
	mimeLower := strings.ToLower(strings.TrimSpace(mimeType))

	if strings.HasPrefix(mimeLower, "image/") {
		return KindImage, ""
	}
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return KindImage, ""
	case ".csv", ".tsv":
		return KindCSV, "CSV"
	case ".json":
		return KindJSON, "JSON"
	case ".docx":
		return KindDOCX, "DOCX"
	case ".pdf":
		return KindPDF, "PDF"
	case ".txt", ".md", ".markdown", ".rst", ".log", ".ini", ".cfg", ".conf":
		return KindText, "Text"
	}

	if lang, ok := codeExtensions[ext]; ok {
		return KindCode, lang
	}
	if strings.EqualFold(clean, "Dockerfile") || strings.EqualFold(clean, "Makefile") {
		return KindCode, clean
	}
	if strings.HasPrefix(mimeLower, "text/") {
		return KindText, "Text"
	}
	return KindUnknown, ""
}

// Process reads at most maxBytes from r, extracts structured content based on
// the file type, and prepares it for AI analysis.
func Process(filename, mimeType string, r io.Reader, maxBytes int64) (ExtractedFile, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileSize
	}

	cleanName := security.SanitizeFilename(filename)
	kind, lang := DetectKind(cleanName, mimeType)
	if kind == KindUnknown {
		return ExtractedFile{}, fmt.Errorf("unsupported file type for %q", cleanName)
	}

	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return ExtractedFile{}, fmt.Errorf("reading file: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return ExtractedFile{}, fmt.Errorf("file exceeds maximum size limit of %d MB", maxBytes>>20)
	}

	out := ExtractedFile{
		Filename: cleanName,
		Kind:     kind,
		Language: lang,
		Size:     int64(len(data)),
	}

	switch kind {
	case KindCSV:
		summary, preview, err := analyzeCSV(data, strings.HasSuffix(strings.ToLower(cleanName), ".tsv"))
		if err != nil {
			return ExtractedFile{}, err
		}
		out.Summary = summary
		out.Content = clampRunes(preview, maxPromptContentChars)

	case KindJSON:
		summary, preview, err := analyzeJSON(data)
		if err != nil {
			return ExtractedFile{}, err
		}
		out.Summary = summary
		out.Content = clampRunes(preview, maxPromptContentChars)

	case KindDOCX:
		text, err := extractDOCX(data)
		if err != nil {
			return ExtractedFile{}, err
		}
		out.Summary = fmt.Sprintf("DOCX document (%d chars extracted)", len(text))
		out.Content = clampRunes(text, maxPromptContentChars)

	case KindPDF:
		text, err := extractPDF(data)
		if err != nil {
			return ExtractedFile{}, err
		}
		out.Summary = fmt.Sprintf("PDF document (%d chars extracted)", len(text))
		out.Content = clampRunes(text, maxPromptContentChars)

	case KindCode, KindText:
		text := strings.TrimSpace(string(data))
		lines := strings.Count(text, "\n") + 1
		out.Summary = fmt.Sprintf("%s file (%d lines, %d bytes)", lang, lines, len(data))
		out.Content = clampRunes(text, maxPromptContentChars)
	}

	return out, nil
}

// BuildPrompt constructs the AI instruction prompt for an extracted file and an
// optional user question/caption.
func BuildPrompt(file ExtractedFile, userCaption string) string {
	caption := strings.TrimSpace(userCaption)
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("File: %s (%s)\n", file.Filename, file.Summary))
	if file.Content != "" {
		sb.WriteString("\n--- File Content ---\n")
		sb.WriteString(file.Content)
		sb.WriteString("\n--- End of File ---\n\n")
	}

	if caption != "" {
		sb.WriteString("User Request: " + caption + "\n")
		sb.WriteString("Answer the user's request based on the file above.")
		return sb.String()
	}

	switch file.Kind {
	case KindCode:
		sb.WriteString("Perform a concise code review of this file: summarize what it does, point out any bugs, race conditions, or security issues, and suggest concrete improvements.")
	case KindCSV:
		sb.WriteString("Analyze this CSV dataset: summarize the structure, key statistics, notable patterns, and anomalies.")
	case KindJSON:
		sb.WriteString("Summarize this JSON structure, its key fields, and notable values.")
	default:
		sb.WriteString("Provide a clear summary, key takeaways, and important details from this document.")
	}

	return sb.String()
}

func analyzeCSV(data []byte, isTSV bool) (string, string, error) {
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	if isTSV {
		reader.Comma = '\t'
	}

	records, err := reader.ReadAll()
	if err != nil && len(records) == 0 {
		return "", "", fmt.Errorf("invalid CSV: %w", err)
	}
	if len(records) == 0 {
		return "Empty CSV file", "", nil
	}

	headers := records[0]
	rows := records[1:]

	type colStat struct {
		count int
		sum   float64
		min   float64
		max   float64
	}
	stats := make([]colStat, len(headers))

	for _, row := range rows {
		for c := 0; c < len(headers) && c < len(row); c++ {
			v, err := strconv.ParseFloat(strings.TrimSpace(row[c]), 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			s := &stats[c]
			if s.count == 0 || v < s.min {
				s.min = v
			}
			if s.count == 0 || v > s.max {
				s.max = v
			}
			s.sum += v
			s.count++
		}
	}

	var statLines []string
	for c, h := range headers {
		s := stats[c]
		if s.count > 0 {
			mean := s.sum / float64(s.count)
			statLines = append(statLines, fmt.Sprintf("- %s: n=%d, min=%.4g, max=%.4g, avg=%.4g", h, s.count, s.min, s.max, mean))
		}
	}

	summary := fmt.Sprintf("CSV (%d rows, %d columns: %s)", len(rows), len(headers), strings.Join(headers, ", "))

	var preview strings.Builder
	preview.WriteString(summary + "\n")
	if len(statLines) > 0 {
		preview.WriteString("Numeric Column Statistics:\n" + strings.Join(statLines, "\n") + "\n\n")
	}
	preview.WriteString("Sample Rows:\n")
	limit := 15
	if len(records) < limit {
		limit = len(records)
	}
	for i := 0; i < limit; i++ {
		preview.WriteString(strings.Join(records[i], " | ") + "\n")
	}

	return summary, preview.String(), nil
}

func analyzeJSON(data []byte) (string, string, error) {
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", "", fmt.Errorf("invalid JSON: %w", err)
	}

	summary := "JSON document"
	switch v := parsed.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		summary = fmt.Sprintf("JSON object (%d top-level keys: %s)", len(keys), strings.Join(keys, ", "))
	case []any:
		summary = fmt.Sprintf("JSON array (%d elements)", len(v))
	}

	pretty, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		pretty = data
	}
	return summary, string(pretty), nil
}

func extractDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("invalid DOCX archive: %w", err)
	}

	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()

		xmlBytes, err := io.ReadAll(io.LimitReader(rc, 4<<20))
		if err != nil {
			return "", err
		}

		decoder := xml.NewDecoder(bytes.NewReader(xmlBytes))
		var sb strings.Builder
		for {
			tok, err := decoder.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				break
			}
			switch se := tok.(type) {
			case xml.StartElement:
				if se.Name.Local == "p" && sb.Len() > 0 {
					sb.WriteByte('\n')
				}
			case xml.CharData:
				sb.Write(se)
			}
		}

		text := strings.TrimSpace(sb.String())
		if text == "" {
			return "", errors.New("DOCX contains no readable text")
		}
		return text, nil
	}

	return "", errors.New("word/document.xml not found in DOCX")
}

var (
	pdfStreamRe  = regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	pdfLiteralRe = regexp.MustCompile(`\(([^()\\]*(?:\\.[^()\\]*)*)\)`)
)

func extractPDF(data []byte) (string, error) {
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		return "", errors.New("invalid PDF header")
	}

	var sb strings.Builder

	extractLiterals := func(buf []byte) {
		matches := pdfLiteralRe.FindAllSubmatch(buf, -1)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			s := cleanPDFString(string(m[1]))
			if s != "" {
				sb.WriteString(s)
				sb.WriteByte(' ')
			}
		}
	}

	streams := pdfStreamRe.FindAllSubmatch(data, -1)
	for _, st := range streams {
		if len(st) < 2 {
			continue
		}
		rawStream := st[1]
		if zr, err := zlib.NewReader(bytes.NewReader(rawStream)); err == nil {
			inflated, _ := io.ReadAll(io.LimitReader(zr, 1<<20))
			_ = zr.Close()
			if len(inflated) > 0 {
				extractLiterals(inflated)
				continue
			}
		}
		extractLiterals(rawStream)
	}

	if sb.Len() == 0 {
		extractLiterals(data)
	}

	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", errors.New("no extractable text found in PDF (it may be scanned/image-only)")
	}
	return text, nil
}

func cleanPDFString(s string) string {
	s = strings.NewReplacer(`\(`, `(`, `\)`, `)`, `\\`, `\`, `\n`, "\n", `\r`, " ").Replace(s)
	var b strings.Builder
	printable := 0
	for _, r := range s {
		if unicode.IsPrint(r) || r == '\n' || r == '\t' {
			b.WriteRune(r)
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				printable++
			}
		}
	}
	if printable == 0 {
		return ""
	}
	return strings.TrimSpace(b.String())
}

func clampRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "\n… (truncated)"
}
