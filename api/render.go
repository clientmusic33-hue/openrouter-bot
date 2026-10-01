package api

import (
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"openrouter-bot/config"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// editInterval throttles Telegram edits while streaming. Telegram API calls
// are relatively expensive; 350ms feels responsive without hammering it.
const editInterval = 350 * time.Millisecond

// maxChunks caps how many Telegram messages a single answer may occupy.
const maxChunks = 20

// maxChunksDefault is used when SplitMessage is called without a cap.
const maxChunksDefault = 20

// typingInterval is how often the "typing…" chat action is refreshed.
// Telegram clears it after about five seconds.
const typingInterval = 5 * time.Second

// -----------------------------------------------------------------------------
// CHUNKED MESSAGE RENDERING
// -----------------------------------------------------------------------------

// messageSink renders a growing answer across as many Telegram messages as it
// needs. Telegram caps a single message at 4096 characters, and that limit is
// enforced on edits too, so a long answer has to spill into new messages.
type messageSink struct {
	bot        *tgbotapi.BotAPI
	chatID     int64
	messageIDs []int
	rendered   []string
	limit      int
	maxChunks  int
	parseMode  string
}

func newMessageSink(bot *tgbotapi.BotAPI, chatID int64, firstMessageID, limit int) *messageSink {
	if limit <= 0 {
		limit = config.ChunkLimit
	}

	return &messageSink{
		bot:        bot,
		chatID:     chatID,
		messageIDs: []int{firstMessageID},
		rendered:   []string{""},
		limit:      limit,
		maxChunks:  maxChunks,
	}
}

// Messages returns the ids of the messages that carry the answer.
func (s *messageSink) Messages() []int {
	return s.messageIDs
}

// Update renders text, sending extra messages when it outgrows the current
// ones. It reports whether anything changed.
func (s *messageSink) Update(text string) bool {
	chunks := SplitMessage(HumanizeMath(text), s.limit, s.maxChunks)
	if len(chunks) == 0 {
		return false
	}

	changed := false

	for i, chunk := range chunks {
		if i >= len(s.messageIDs) {
			msg := tgbotapi.NewMessage(s.chatID, chunk)
			sent, err := s.bot.Send(msg)
			if err != nil {
				log.Printf("Failed to send continuation message: %v", err)
				return changed
			}
			s.messageIDs = append(s.messageIDs, sent.MessageID)
			s.rendered = append(s.rendered, chunk)
			changed = true

			continue
		}

		if chunk == s.rendered[i] {
			continue
		}

		if editMessage(s.bot, s.chatID, s.messageIDs[i], chunk, "") {
			s.rendered[i] = chunk
			changed = true
		}
	}

	return changed
}

// Finish writes the final text. When markdown is enabled it upgrades the
// formatting, trying MarkdownV2 and then legacy Markdown before settling for
// the plain text Update already wrote.
func (s *messageSink) Finish(text string, markdown bool) {
	text = HumanizeMath(text)
	s.Update(text)

	if !markdown {
		return
	}

	chunks := SplitMessage(text, s.limit, s.maxChunks)

	for i, chunk := range chunks {
		if i >= len(s.messageIDs) {
			return
		}

		// Markdown is only applied at the end: a half-written "**bold" would
		// be rejected by Telegram while streaming.
		if editMessage(s.bot, s.chatID, s.messageIDs[i], chunk, tgbotapi.ModeMarkdownV2) {
			continue
		}
		if editMessage(s.bot, s.chatID, s.messageIDs[i], chunk, tgbotapi.ModeMarkdown) {
			continue
		}

		// Leave the plain text version that Update already wrote.
		log.Printf("Markdown rendering failed for chunk %d, keeping plain text", i)
	}
}

// SetKeyboard attaches a keyboard to the first message of the answer. The
// buttons live on the placeholder, which is the message the user is looking
// at while the answer streams in.
func (s *messageSink) SetKeyboard(keyboard tgbotapi.InlineKeyboardMarkup) {
	if len(s.messageIDs) == 0 {
		return
	}

	edit := tgbotapi.NewEditMessageReplyMarkup(s.chatID, s.messageIDs[0], keyboard)
	if _, err := s.bot.Send(edit); err != nil && !strings.Contains(err.Error(), "message is not modified") {
		log.Printf("Failed to attach keyboard: %v", err)
	}
}

func editMessage(
	bot *tgbotapi.BotAPI,
	chatID int64,
	messageID int,
	text string,
	parseMode string,
) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	if parseMode != "" {
		editMsg.ParseMode = parseMode
	}

	if _, err := bot.Send(editMsg); err != nil {
		if !strings.Contains(err.Error(), "message is not modified") {
			log.Printf("Telegram edit error (parse_mode=%q): %v", parseMode, err)
		}
		return false
	}

	return true
}

// SplitMessage breaks text into chunks of at most limit runes, preferring
// newline boundaries so that paragraphs and code blocks stay intact.
func SplitMessage(text string, limit int, maxChunks int) []string {
	if limit <= 0 {
		limit = config.ChunkLimit
	}
	if maxChunks <= 0 {
		maxChunks = maxChunksDefault
	}

	text = strings.TrimRight(text, " \t\n")
	if text == "" {
		return nil
	}

	runes := []rune(text)
	if len(runes) <= limit {
		return []string{text}
	}

	var chunks []string

	for start := 0; start < len(runes); {
		if len(chunks) == maxChunks-1 {
			// Last allowed chunk: keep the tail and mark the truncation.
			tail := strings.TrimRight(string(runes[start:]), " \t\n")
			chunks = append(chunks, tail+"\n\n… (truncated)")
			break
		}

		end := start + limit
		if end >= len(runes) {
			chunks = append(chunks, strings.TrimRight(string(runes[start:]), " \t\n"))
			break
		}

		cut := -1
		for i := end; i > start+limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i
				break
			}
		}
		if cut <= 0 {
			cut = end
		}

		chunk := strings.TrimRight(string(runes[start:cut]), " \t\n")
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		start = cut
	}

	if len(chunks) == 0 {
		return nil
	}

	return chunks
}

// -----------------------------------------------------------------------------
// MATH RENDERING
// -----------------------------------------------------------------------------

// Models answer mathematical questions in LaTeX, because that is what they
// were trained on, and Telegram has no LaTeX renderer: the user receives
// "\frac{-b \pm \sqrt{b^2-4ac}}{2a}" where they expected an equation. These
// rules rewrite the notation models actually emit into characters a phone can
// draw, so an answer reads like maths instead of like source code.
//
// The rewrite is deliberately conservative. Text that only looks like maths by
// accident - a price such as "$5 and $10", an identifier such as "user_id" -
// is left alone, and anything the rules cannot map is passed through untouched
// rather than mangled.

// mathSymbolMap maps the LaTeX commands worth knowing to a single printable
// character. Anything not listed here is left as the model wrote it.
var mathSymbolMap = map[string]string{
	// operators and relations
	"times": "×", "cdot": "·", "div": "÷", "pm": "±", "mp": "∓",
	"neq": "≠", "ne": "≠", "leq": "≤", "le": "≤", "geq": "≥", "ge": "≥",
	"approx": "≈", "equiv": "≡", "propto": "∝", "ll": "≪", "gg": "≫",
	"sum": "∑", "prod": "∏", "int": "∫", "oint": "∮", "iint": "∬",
	"partial": "∂", "nabla": "∇", "infty": "∞", "degree": "°", "circ": "°",
	"deg":        "°",
	"rightarrow": "→", "to": "→", "leftarrow": "←", "Rightarrow": "⇒",
	"Leftarrow": "⇐", "leftrightarrow": "↔", "Leftrightarrow": "⇔",
	"dots": "…", "ldots": "…", "cdots": "⋯", "angle": "∠",
	"perp": "⊥", "parallel": "∥", "prime": "′", "therefore": "∴",
	"because": "∵", "forall": "∀", "exists": "∃", "in": "∈",
	"notin": "∉", "subset": "⊂", "supset": "⊃", "cup": "∪", "cap": "∩",
	"emptyset": "∅", "star": "★",

	// greek
	"alpha": "α", "beta": "β", "gamma": "γ", "delta": "δ",
	"epsilon": "ε", "varepsilon": "ε", "zeta": "ζ", "eta": "η",
	"theta": "θ", "vartheta": "θ", "iota": "ι", "kappa": "κ",
	"lambda": "λ", "mu": "μ", "nu": "ν", "xi": "ξ", "pi": "π",
	"rho": "ρ", "sigma": "σ", "tau": "τ", "upsilon": "υ", "phi": "φ",
	"varphi": "φ", "chi": "χ", "psi": "ψ", "omega": "ω",
	"Gamma": "Γ", "Delta": "Δ", "Theta": "Θ", "Lambda": "Λ", "Xi": "Ξ",
	"Pi": "Π", "Sigma": "Σ", "Upsilon": "Υ", "Phi": "Φ", "Psi": "Ψ",
	"Omega": "Ω",
}

// vulgarFractions renders the handful of fractions that have their own
// character, so "1/2" reads as ½ the way a textbook prints it.
var vulgarFractions = map[string]string{
	"1/2": "½", "1/3": "⅓", "2/3": "⅔", "1/4": "¼", "3/4": "¾",
	"1/5": "⅕", "1/6": "⅙", "1/8": "⅛", "3/8": "⅜", "5/8": "⅝",
	"7/8": "⅞",
}

// vulgarFractionChars collects the characters above, so a fraction inside a
// fraction can be recognised and parenthesised.
var vulgarFractionChars = func() map[rune]bool {
	set := make(map[rune]bool, len(vulgarFractions))
	for _, fraction := range vulgarFractions {
		for _, r := range fraction {
			set[r] = true
		}
	}

	return set
}()

var (
	// A LaTeX command name: the letters right after the backslash. Matching
	// the whole run of letters keeps "\alpha\beta" as two commands and keeps
	// a false friend like "\Users" from being read as "\nu" + "sers".
	mathSymbolRe = regexp.MustCompile(`\\([a-zA-Z]+)`)

	// Display maths is set on its own line: \[ ... \] and $$ ... $$. The
	// spaces around the fences go with them, so the seam stays clean.
	mathDisplayBracketRe = regexp.MustCompile(`(?s)[ \t]*\\\[[ \t]*(.*?)[ \t]*\\\][ \t]*`)
	mathDisplayDollarRe  = regexp.MustCompile(`[ \t]*\$\$[ \t]*((?:[^\\\$]|\\[\s\S])*?)[ \t]*\$\$[ \t]*`)

	// Inline maths: \( ... \) is unambiguous, $ ... $ needs a maths check.
	mathInlineParenRe  = regexp.MustCompile(`(?s)\\\((.*?)\\\)`)
	mathInlineDollarRe = regexp.MustCompile(`\$((?:[^\\\$]|\\[\s\S])*)\$`)

	// Environments and their contents are kept; only the fences go.
	mathEnvRe = regexp.MustCompile(`\\(?:begin|end)\{[a-zA-Z*]+\}`)

	// Wrappers that only change the font or draw a box: keep the contents.
	mathWrapperRe = regexp.MustCompile(
		`\\(?:text|textrm|mathrm|mathbf|mathit|mathsf|mathtt|boldsymbol|` +
			`mathbb|mathcal|mathfrak|operatorname|boxed|overline|underline|` +
			`widehat|hat|vec|bar|tilde)\{([^{}]*)\}`)

	// \left( and \right) are sizing hints that mean nothing in plain text.
	mathSizingRe = regexp.MustCompile(`\\(?:left|right)\s*([()\[\]|])|\\(?:left|right|big|Big|bigg|Bigg)\b`)

	// Spacing commands.
	mathSpacingRe = regexp.MustCompile(`\\(?:displaystyle|limits|nolimits|quad|qquad|,|;|:|!| )`)

	// \binom{n}{k} has no single character, so it is spelled out.
	mathBinomRe = regexp.MustCompile(`\\(?:binom|dbinom|tbinom)\{([^{}]*)\}\{([^{}]*)\}`)

	mathFracRe = regexp.MustCompile(`\\(?:frac|dfrac|tfrac|cfrac)\{([^{}]*)\}\{([^{}]*)\}`)

	mathRootNthRe  = regexp.MustCompile(`\\sqrt\[([^\]]*)\]\{([^{}]*)\}`)
	mathRootRe     = regexp.MustCompile(`\\sqrt\{([^{}]*)\}`)
	mathRootBareRe = regexp.MustCompile(`\\sqrt\s*([0-9A-Za-z])`)

	mathSuperBraceRe = regexp.MustCompile(`\^\{([^{}]*)\}`)
	mathSuperPlainRe = regexp.MustCompile(`\^(-?\d+|[A-Za-z])`)
	mathSubBraceRe   = regexp.MustCompile(`_\{([^{}]*)\}`)
	// A bare subscript only counts after something it can belong to, so
	// "user_id" survives while "x_1" becomes x₁.
	mathSubPlainRe = regexp.MustCompile(`([A-Za-z0-9)])_(\d+)`)

	// A single letter subscript is maths ("x_i"), but only when it is not
	// the start of a word: "user_id" stays an identifier, "x_i" becomes xᵢ.
	mathSubLetterRe = regexp.MustCompile(`([A-Za-z0-9)])_([A-Za-z0-9])([^A-Za-z0-9]|$)`)

	// A fragment a stream can leave dangling at the very end of an answer:
	// "\fra" from \frac, or a "\(" whose partner has not arrived yet.
	trailingPartialRe = regexp.MustCompile(`\\(?:[a-zA-Z]*|[(\[])$`)

	// Escaped literals: \% \$ \& \# \_
	mathEscapeRe = regexp.MustCompile(`\\([%$&#_])`)

	// Alignment tabs left over from a matrix, only when they stand alone.
	mathAlignRe = regexp.MustCompile(`(?m)(?:^|\s)&(?:\s|$)`)

	newlineRunRe = regexp.MustCompile(`\n{3,}`)
)

// superscript and subscript alphabets.
const (
	superDigits = "⁰¹²³⁴⁵⁶⁷⁸⁹"
	subDigits   = "₀₁₂₃₄₅₆₇₈₉"
)

var superExtra = map[rune]rune{
	'+': '⁺', '-': '⁻', '=': '⁼', '(': '⁽', ')': '⁾',
	'n': 'ⁿ', 'i': 'ⁱ', 'T': 'ᵀ',
}

var subExtra = map[rune]rune{
	'+': '₊', '-': '₋', '=': '₌', '(': '₍', ')': '₎',
	'a': 'ₐ', 'e': 'ₑ', 'h': 'ₕ', 'i': 'ᵢ', 'j': 'ⱼ', 'k': 'ₖ',
	'l': 'ₗ', 'm': 'ₘ', 'n': 'ₙ', 'o': 'ₒ', 'p': 'ₚ', 'r': 'ᵣ',
	's': 'ₛ', 't': 'ₜ', 'u': 'ᵤ', 'v': 'ᵥ', 'x': 'ₓ',
}

// HumanizeMath rewrites mathematical notation into readable plain text. It is
// safe to call on any text: ordinary prose comes back unchanged.
func HumanizeMath(text string) string {
	// Cheap guard: most answers never touch this machinery.
	if !strings.ContainsAny(text, `\^_$`) {
		return text
	}

	out := trimPartialMath(text)

	// Delimiters first: display maths on its own line, inline maths bare.
	out = mathDisplayBracketRe.ReplaceAllString(out, "\n$1\n")
	out = mathDisplayDollarRe.ReplaceAllStringFunc(out, unwrapDisplayDollar)
	out = mathInlineParenRe.ReplaceAllString(out, "$1")
	out = mathInlineDollarRe.ReplaceAllStringFunc(out, unwrapInlineDollar)
	out = mathEnvRe.ReplaceAllString(out, "")

	// One pass is not enough. \frac{-b \pm \sqrt{b^2-4ac}}{2a} only becomes
	// readable once the root inside it has collapsed to plain characters, so
	// the rules run until the text stops changing.
	for i := 0; i < 12; i++ {
		before := out

		out = expandWrappers(out)
		out = expandRoots(out)
		out = mathBinomRe.ReplaceAllString(out, "($1 choose $2)")
		out = expandFractions(out)
		out = expandScripts(out)
		out = expandSymbols(out)
		out = mathSizingRe.ReplaceAllStringFunc(out, keepSizing)

		if out == before {
			break
		}
	}

	// What is left is LaTeX layout: line breaks, escapes and spacing.
	out = strings.ReplaceAll(out, `\\`, "\n")
	out = mathEscapeRe.ReplaceAllString(out, "$1")
	out = mathSpacingRe.ReplaceAllString(out, " ")
	out = mathAlignRe.ReplaceAllString(out, "  ")
	out = newlineRunRe.ReplaceAllString(out, "\n\n")

	return out
}

// unwrapDisplayDollar strips $$ ... $$ when the contents look like maths, so
// that a price list keeps its dollars.
func unwrapDisplayDollar(match string) string {
	inner := strings.TrimSpace(match)
	inner = strings.TrimPrefix(inner, "$$")
	inner = strings.TrimSuffix(inner, "$$")
	inner = strings.TrimSpace(inner)

	if !looksLikeMath(inner) {
		return match
	}

	return "\n" + inner + "\n"
}

// unwrapInlineDollar does the same for $ ... $, which is the riskier form
// because a dollar sign is also currency.
func unwrapInlineDollar(match string) string {
	inner := strings.TrimSuffix(strings.TrimPrefix(match, "$"), "$")

	if len(inner) > 300 || !looksLikeMath(inner) {
		return match
	}

	return inner
}

// looksLikeMath reports whether a fragment carries notation that only maths
// uses: a LaTeX command, an exponent, a subscript or an operator.
func looksLikeMath(s string) bool {
	for _, r := range s {
		switch r {
		case '\\', '^', '_', '=', '+', '*', '/', '<', '>':
			return true
		}
	}

	return false
}

// expandSymbols replaces every known LaTeX command with one character.
func expandSymbols(s string) string {
	return mathSymbolRe.ReplaceAllStringFunc(s, func(m string) string {
		if symbol, ok := mathSymbolMap[m[1:]]; ok {
			return symbol
		}

		return m
	})
}

// expandFractions turns \frac{a}{b} into a/b, or into ½ when the fraction has
// a character of its own. It repeats so that nested fractions collapse from
// the inside out.
func expandFractions(s string) string {
	for i := 0; i < 10 && mathFracRe.MatchString(s); i++ {
		s = mathFracRe.ReplaceAllStringFunc(s, func(m string) string {
			groups := mathFracRe.FindStringSubmatch(m)
			if len(groups) < 3 {
				return m
			}

			return renderFraction(groups[1], groups[2])
		})
	}

	return s
}

// renderFraction writes one fraction. Compound parts are parenthesised so
// that (a+b)/(c+d) cannot be misread as a + b/c + d.
func renderFraction(numerator, denominator string) string {
	num := strings.TrimSpace(numerator)
	den := strings.TrimSpace(denominator)

	if vulgar, ok := vulgarFractions[num+"/"+den]; ok {
		return vulgar
	}

	if needsParentheses(num) {
		num = "(" + num + ")"
	}
	if needsParentheses(den) {
		den = "(" + den + ")"
	}

	return num + "/" + den
}

// needsParentheses reports whether a fraction part would change meaning
// without them.
func needsParentheses(part string) bool {
	if strings.ContainsAny(part, "+-*/ ") || strings.Contains(part, "\\") {
		return true
	}

	// A fraction of fractions needs the grouping to stay readable.
	for _, r := range part {
		if vulgarFractionChars[r] {
			return true
		}
	}

	return false
}

// expandRoots turns \sqrt{x} into √(x).
func expandRoots(s string) string {
	s = mathRootNthRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := mathRootNthRe.FindStringSubmatch(m)
		if len(groups) < 3 {
			return m
		}

		switch groups[1] {
		case "3":
			return "∛(" + groups[2] + ")"
		case "4":
			return "∜(" + groups[2] + ")"
		default:
			return "√[" + groups[1] + "](" + groups[2] + ")"
		}
	})
	s = mathRootRe.ReplaceAllString(s, "√($1)")
	s = mathRootBareRe.ReplaceAllString(s, "√$1")

	return s
}

// expandWrappers drops the commands that only change the typeface, keeping
// what they wrap. It repeats so that \text{\boxed{x}} unwinds too.
func expandWrappers(s string) string {
	for i := 0; i < 10 && mathWrapperRe.MatchString(s); i++ {
		s = mathWrapperRe.ReplaceAllString(s, "$1")
	}

	return s
}

// keepSizing keeps the bracket a \left or \right was sizing and drops the
// command itself.
func keepSizing(m string) string {
	if groups := mathSizingRe.FindStringSubmatch(m); len(groups) > 1 && groups[1] != "" {
		return groups[1]
	}

	return ""
}

// expandScripts converts powers and indices to superscript and subscript
// characters, which every phone font carries.
func expandScripts(s string) string {
	s = mathSuperBraceRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := mathSuperBraceRe.FindStringSubmatch(m)
		if len(groups) < 2 {
			return m
		}

		if raised, ok := toScript(groups[1], superDigits, superExtra); ok {
			return raised
		}

		return "^(" + groups[1] + ")"
	})

	s = mathSuperPlainRe.ReplaceAllStringFunc(s, func(m string) string {
		raised, ok := toScript(strings.TrimPrefix(m, "^"), superDigits, superExtra)
		if !ok {
			return m
		}

		return raised
	})

	s = mathSubBraceRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := mathSubBraceRe.FindStringSubmatch(m)
		if len(groups) < 2 {
			return m
		}

		if lowered, ok := toScript(groups[1], subDigits, subExtra); ok {
			return lowered
		}

		return "_(" + groups[1] + ")"
	})

	s = mathSubPlainRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := mathSubPlainRe.FindStringSubmatch(m)
		if len(groups) < 3 {
			return m
		}

		lowered, ok := toScript(groups[2], subDigits, subExtra)
		if !ok {
			return m
		}

		return groups[1] + lowered
	})

	s = mathSubLetterRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := mathSubLetterRe.FindStringSubmatch(m)
		if len(groups) < 4 {
			return m
		}

		lowered, ok := toScript(groups[2], subDigits, subExtra)
		if !ok {
			return m
		}

		return groups[1] + lowered + groups[3]
	})

	return s
}

// trimPartialMath hides the tail of a command that a stream has not finished
// delivering yet. While an answer is still arriving the sink renders whatever
// it has, and a half-written "\fra" or a lone "\(" is noise the user would
// only see for a moment; withholding it costs nothing because the next update
// carries the complete token.
//
// Only fragments of commands this file knows are withheld, so a trailing word
// such as "C:\Users" is left exactly as written.
func trimPartialMath(s string) string {
	// One fragment can hide another: withholding "\sqr" may leave a "\("
	// that is just as much a fragment, so this repeats until it settles.
	for i := 0; i < 5; i++ {
		match := trailingPartialRe.FindString(s)
		if match == "" {
			return s
		}

		tail := match[1:]
		if tail != "" && tail != "(" && tail != "[" && !isKnownCommandPrefix(tail) {
			return s
		}

		s = strings.TrimRight(s[:len(s)-len(match)], " ")
	}

	return s
}

// mathCommandNames collects every command this file understands, so a partial
// match can be recognised before the rest of the word arrives.
var mathCommandNames = func() []string {
	names := make([]string, 0, len(mathSymbolMap)+32)
	for name := range mathSymbolMap {
		names = append(names, name)
	}

	names = append(names,
		"frac", "dfrac", "tfrac", "cfrac", "sqrt", "binom", "dbinom", "tbinom",
		"text", "textrm", "mathrm", "mathbf", "mathit", "mathsf", "mathtt",
		"boldsymbol", "mathbb", "mathcal", "mathfrak", "operatorname",
		"boxed", "overline", "underline", "widehat", "hat", "vec", "bar",
		"tilde", "left", "right", "begin", "end", "displaystyle", "limits",
		"nolimits", "quad", "qquad",
	)

	return names
}()

// isKnownCommandPrefix reports whether a fragment could still grow into a
// command this file rewrites. A complete command is not a prefix of itself:
// an answer ending in "\pi" has arrived, one ending in "\fra" has not.
func isKnownCommandPrefix(fragment string) bool {
	if mathCommandSet[fragment] {
		return false
	}

	for _, name := range mathCommandNames {
		if strings.HasPrefix(name, fragment) {
			return true
		}
	}

	return false
}

// mathCommandSet is the same list as a set, for the completeness check above.
var mathCommandSet = func() map[string]bool {
	set := make(map[string]bool, len(mathCommandNames))
	for _, name := range mathCommandNames {
		set[name] = true
	}

	return set
}()

// toScript maps a run of characters onto a superscript or subscript alphabet.
// It reports false when any character has no such form, so the caller can
// fall back to the plain notation instead of half-converting.
func toScript(s string, digits string, extra map[rune]rune) (string, bool) {
	if s == "" {
		return "", false
	}

	out := make([]rune, 0, len(s))

	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			out = append(out, []rune(digits)[r-'0'])
		default:
			mapped, ok := extra[r]
			if !ok {
				return "", false
			}

			out = append(out, mapped)
		}
	}

	return string(out), true
}

// -----------------------------------------------------------------------------
// LOADING FEEDBACK
// -----------------------------------------------------------------------------

// runLoadingAnimation keeps editing the placeholder while the model thinks.
// Without it a slow request looks like a frozen chat.
func runLoadingAnimation(
	bot *tgbotapi.BotAPI,
	chatID int64,
	messageID int,
	loadMessage string,
	stop chan bool,
) {
	dots := []string{"", ".", "..", "..."}

	ticker := time.NewTicker(1200 * time.Millisecond)
	defer ticker.Stop()

	for i := 0; ; i = (i + 1) % len(dots) {
		select {
		case <-stop:
			return
		case <-ticker.C:
			editMsg := tgbotapi.NewEditMessageText(chatID, messageID, loadMessage+dots[i])
			if _, err := bot.Send(editMsg); err != nil {
				if !strings.Contains(err.Error(), "message is not modified") {
					log.Printf("Loading edit error: %v", err)
				}
			}
		}
	}
}

func stopLoading(stopAnimation chan bool) {
	select {
	case stopAnimation <- true:
	default:
	}
}

// typingLoop sends the "typing" chat action until the returned stop function
// is called. It is what makes the bot feel alive in clients that render the
// action instead of the placeholder text.
func typingLoop(bot *tgbotapi.BotAPI, chatID int64) func() {
	done := make(chan struct{})
	var once sync.Once

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		send := func() {
			if _, err := bot.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)); err != nil {
				log.Printf("Failed to send chat action: %v", err)
			}
		}

		send()

		ticker := time.NewTicker(typingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				send()
			}
		}
	}()

	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}
