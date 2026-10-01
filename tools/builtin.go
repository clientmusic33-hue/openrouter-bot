package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"openrouter-bot/features/reminders"
	"openrouter-bot/features/research"
	"openrouter-bot/internal/security"
	"openrouter-bot/provider"
	"openrouter-bot/translator"
)

// DefaultRegistry builds a registry populated with safe default tools:
// calculator, time, web, url, github, translator, and notes.
// Dangerous file/shell tools remain disabled by default.
func DefaultRegistry(chain *provider.Chain, remMgr *reminders.Manager) *Registry {
	reg := NewRegistry()

	_ = reg.Register(Tool{
		Name:        "calculator",
		Description: "Evaluates mathematical expressions (+, -, *, /, ^, %, sqrt, sin, cos, tan, ln, log, abs, pi, e)",
		InputSchema: "math expression string, e.g. 'sqrt(144) + 2^5'",
		Timeout:     2 * time.Second,
		Permission:  PermEveryone,
		Execute: func(_ context.Context, _, input string) (string, error) {
			val, err := EvalMath(input)
			if err != nil {
				return "", err
			}
			return strconv.FormatFloat(val, 'g', 12, 64), nil
		},
	})

	_ = reg.Register(Tool{
		Name:        "time",
		Description: "Returns current UTC date/time or converts an IANA timezone or Unix timestamp",
		InputSchema: "optional timezone name (e.g. 'UTC', 'America/New_York', 'Asia/Kolkata') or unix seconds",
		Timeout:     2 * time.Second,
		Permission:  PermEveryone,
		Execute: func(_ context.Context, _, input string) (string, error) {
			now := time.Now().UTC()
			arg := strings.TrimSpace(input)
			if arg == "" || strings.EqualFold(arg, "utc") || strings.EqualFold(arg, "now") {
				return now.Format(time.RFC3339), nil
			}
			if unixSec, err := strconv.ParseInt(arg, 10, 64); err == nil {
				return time.Unix(unixSec, 0).UTC().Format(time.RFC3339), nil
			}
			loc, err := time.LoadLocation(arg)
			if err != nil {
				return now.Format(time.RFC3339) + " (UTC fallback)", nil
			}
			return now.In(loc).Format(time.RFC3339), nil
		},
	})

	_ = reg.Register(Tool{
		Name:        "url",
		Description: "Fetches a public HTTP/HTTPS URL and extracts its readable text (SSRF protected)",
		InputSchema: "full https:// URL",
		Timeout:     12 * time.Second,
		Permission:  PermUser,
		Execute: func(ctx context.Context, _, input string) (string, error) {
			src, err := research.FetchPage(ctx, nil, input, 2000)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Title: %s\nURL: %s\nContent:\n%s", src.Title, src.URL, src.Snippet), nil
		},
	})

	_ = reg.Register(Tool{
		Name:        "web",
		Description: "Searches the web/reference sources for a query and returns deduplicated snippets",
		InputSchema: "search query string",
		Timeout:     12 * time.Second,
		Permission:  PermUser,
		Execute: func(ctx context.Context, _, input string) (string, error) {
			sources, err := research.Search(ctx, nil, input, 3)
			if err != nil {
				return "", err
			}
			if len(sources) == 0 {
				return "No external results found.", nil
			}
			var sb strings.Builder
			for i, s := range sources {
				sb.WriteString(fmt.Sprintf("[%d] %s (%s)\n%s\n\n", i+1, s.Title, s.URL, s.Snippet))
			}
			return strings.TrimSpace(sb.String()), nil
		},
	})

	_ = reg.Register(Tool{
		Name:        "github",
		Description: "Fetches public GitHub repository metadata (stars, language, description, open issues)",
		InputSchema: "owner/repo (e.g. 'golang/go')",
		Timeout:     10 * time.Second,
		Permission:  PermUser,
		Execute: func(ctx context.Context, _, input string) (string, error) {
			return lookupGitHubRepo(ctx, input)
		},
	})

	_ = reg.Register(Tool{
		Name:        "translator",
		Description: "Translates text into a target language using the AI provider chain",
		InputSchema: "<target_language>: <text>",
		Timeout:     25 * time.Second,
		Permission:  PermEveryone,
		Execute: func(ctx context.Context, _, input string) (string, error) {
			if chain == nil {
				return "", errors.New("no provider chain available")
			}
			parts := strings.SplitN(input, ":", 2)
			if len(parts) < 2 {
				return translator.Translate(ctx, chain, input, "English")
			}
			return translator.Translate(ctx, chain, strings.TrimSpace(parts[1]), strings.TrimSpace(parts[0]))
		},
	})

	_ = reg.Register(Tool{
		Name:        "notes",
		Description: "Lists or adds persistent notes for the current user",
		InputSchema: "'list' or 'add: <note text>'",
		Timeout:     3 * time.Second,
		Permission:  PermEveryone,
		Execute: func(_ context.Context, userID, input string) (string, error) {
			if remMgr == nil {
				return "Notes storage is not configured.", nil
			}
			trimmed := strings.TrimSpace(input)
			lower := strings.ToLower(trimmed)
			if strings.HasPrefix(lower, "add:") {
				note, err := remMgr.AddNote(userID, strings.TrimSpace(trimmed[4:]))
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Saved note #%d: %s", note.ID, note.Text), nil
			}
			notes := remMgr.ListNotes(userID)
			if len(notes) == 0 {
				return "No saved notes.", nil
			}
			var sb strings.Builder
			for _, n := range notes {
				sb.WriteString(fmt.Sprintf("#%d: %s\n", n.ID, n.Text))
			}
			return strings.TrimSpace(sb.String()), nil
		},
	})

	// Host file tool is registered as disabled by default for safety.
	_ = reg.Register(Tool{
		Name:        "file",
		Description: "Reads local host files (disabled by default for security)",
		InputSchema: "relative file path",
		Timeout:     3 * time.Second,
		Permission:  PermDisabled,
		Execute: func(_ context.Context, _, _ string) (string, error) {
			return "", errors.New("file tool is disabled by security policy")
		},
	})

	return reg
}

func lookupGitHubRepo(ctx context.Context, repo string) (string, error) {
	repo = strings.TrimSpace(strings.TrimPrefix(repo, "https://github.com/"))
	repo = strings.Trim(repo, "/")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errors.New("expected owner/repo format, e.g. golang/go")
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s", parts[0], parts[1])
	if _, err := security.ValidateExternalURL(apiURL); err != nil {
		return "", err
	}

	client := security.SafeHTTPClient(8 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "OpenRouterTelegramBot/1.0")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return "", err
	}

	var data struct {
		FullName    string `json:"full_name"`
		Description string `json:"description"`
		Language    string `json:"language"`
		Stars       int    `json:"stargazers_count"`
		Forks       int    `json:"forks_count"`
		OpenIssues  int    `json:"open_issues_count"`
		HTMLURL     string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", err
	}

	return fmt.Sprintf(
		"Repository: %s\nDescription: %s\nLanguage: %s | Stars: %d | Forks: %d | Open Issues: %d\nURL: %s",
		data.FullName, data.Description, data.Language, data.Stars, data.Forks, data.OpenIssues, data.HTMLURL,
	), nil
}

// -----------------------------------------------------------------------------
// SAFE RECURSIVE-DESCENT MATH EVALUATOR
// -----------------------------------------------------------------------------

type mathParser struct {
	input string
	pos   int
}

// EvalMath safely evaluates a mathematical expression without executing code.
func EvalMath(expr string) (float64, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return 0, errors.New("empty math expression")
	}
	if len(expr) > 512 {
		return 0, errors.New("expression too long")
	}

	p := &mathParser{input: expr}
	val, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	p.skipSpaces()
	if p.pos < len(p.input) {
		return 0, fmt.Errorf("unexpected token at position %d", p.pos)
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, errors.New("math result is undefined or infinite")
	}
	return val, nil
}

func (p *mathParser) skipSpaces() {
	for p.pos < len(p.input) && unicode.IsSpace(rune(p.input[p.pos])) {
		p.pos++
	}
}

func (p *mathParser) parseExpr() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpaces()
		if p.pos >= len(p.input) {
			return left, nil
		}
		op := p.input[p.pos]
		if op != '+' && op != '-' {
			return left, nil
		}
		p.pos++
		right, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if op == '+' {
			left += right
		} else {
			left -= right
		}
	}
}

func (p *mathParser) parseTerm() (float64, error) {
	left, err := p.parsePower()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpaces()
		if p.pos >= len(p.input) {
			return left, nil
		}
		op := p.input[p.pos]
		if op != '*' && op != '/' && op != '%' {
			return left, nil
		}
		p.pos++
		right, err := p.parsePower()
		if err != nil {
			return 0, err
		}
		switch op {
		case '*':
			left *= right
		case '/':
			if right == 0 {
				return 0, errors.New("division by zero")
			}
			left /= right
		case '%':
			if right == 0 {
				return 0, errors.New("modulo by zero")
			}
			left = math.Mod(left, right)
		}
	}
}

func (p *mathParser) parsePower() (float64, error) {
	base, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	p.skipSpaces()
	if p.pos < len(p.input) && p.input[p.pos] == '^' {
		p.pos++
		exp, err := p.parsePower()
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exp), nil
	}
	return base, nil
}

func (p *mathParser) parseUnary() (float64, error) {
	p.skipSpaces()
	if p.pos < len(p.input) {
		if p.input[p.pos] == '+' {
			p.pos++
			return p.parseUnary()
		}
		if p.input[p.pos] == '-' {
			p.pos++
			v, err := p.parseUnary()
			return -v, err
		}
	}
	return p.parsePrimary()
}

func (p *mathParser) parsePrimary() (float64, error) {
	p.skipSpaces()
	if p.pos >= len(p.input) {
		return 0, errors.New("unexpected end of expression")
	}

	if p.input[p.pos] == '(' {
		p.pos++
		val, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		p.skipSpaces()
		if p.pos >= len(p.input) || p.input[p.pos] != ')' {
			return 0, errors.New("missing closing parenthesis")
		}
		p.pos++
		return val, nil
	}

	start := p.pos
	if unicode.IsLetter(rune(p.input[p.pos])) {
		for p.pos < len(p.input) && (unicode.IsLetter(rune(p.input[p.pos])) || unicode.IsDigit(rune(p.input[p.pos]))) {
			p.pos++
		}
		ident := strings.ToLower(p.input[start:p.pos])
		switch ident {
		case "pi":
			return math.Pi, nil
		case "e":
			return math.E, nil
		}

		p.skipSpaces()
		if p.pos >= len(p.input) || p.input[p.pos] != '(' {
			return 0, fmt.Errorf("unknown constant or function %q", ident)
		}
		p.pos++
		arg, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		p.skipSpaces()
		if p.pos >= len(p.input) || p.input[p.pos] != ')' {
			return 0, errors.New("missing closing parenthesis for function")
		}
		p.pos++

		switch ident {
		case "sqrt":
			return math.Sqrt(arg), nil
		case "sin":
			return math.Sin(arg), nil
		case "cos":
			return math.Cos(arg), nil
		case "tan":
			return math.Tan(arg), nil
		case "ln":
			return math.Log(arg), nil
		case "log":
			return math.Log10(arg), nil
		case "abs":
			return math.Abs(arg), nil
		case "floor":
			return math.Floor(arg), nil
		case "ceil":
			return math.Ceil(arg), nil
		case "round":
			return math.Round(arg), nil
		default:
			return 0, fmt.Errorf("unsupported function %q", ident)
		}
	}

	for p.pos < len(p.input) && (unicode.IsDigit(rune(p.input[p.pos])) || p.input[p.pos] == '.') {
		p.pos++
	}
	if start == p.pos {
		return 0, fmt.Errorf("expected number at position %d", p.pos)
	}
	return strconv.ParseFloat(p.input[start:p.pos], 64)
}
