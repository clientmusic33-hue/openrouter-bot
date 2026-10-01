package api

import "testing"

// TestHumanizeMath pins the notation a model actually emits against the text
// the user should see. Every case is written the way an LLM answers a maths
// question, because that is what has to survive the rewrite.
func TestHumanizeMath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "quadratic formula",
			in:   `x = \frac{-b \pm \sqrt{b^2-4ac}}{2a}`,
			want: "x = (-b ± √(b²-4ac))/2a",
		},
		{
			name: "simple fraction gets its own character",
			in:   `The area is \frac{1}{2} of the base times the height.`,
			want: "The area is ½ of the base times the height.",
		},
		{
			name: "compound parts are parenthesised",
			in:   `\frac{a+b}{c+d}`,
			want: "(a+b)/(c+d)",
		},
		{
			name: "nested fractions collapse from the inside",
			in:   `\frac{1}{\frac{2}{3}}`,
			want: "1/(⅔)",
		},
		{
			name: "inline delimiters disappear",
			in:   `The answer is \(x^2 + 1\) for every x.`,
			want: "The answer is x² + 1 for every x.",
		},
		{
			name: "display maths gets its own line",
			in:   `See this: \[E = mc^2\] and that is all.`,
			want: "See this:\nE = mc²\nand that is all.",
		},
		{
			name: "dollar delimiters disappear",
			in:   `The value is $x^2 + 1$ here.`,
			want: "The value is x² + 1 here.",
		},
		{
			name: "double dollar display",
			in:   `$$\int_0^1 x^2 dx$$`,
			want: "\n∫₀¹ x² dx\n",
		},
		{
			name: "greek letters",
			in:   `\alpha + \beta = \gamma \approx \pi`,
			want: "α + β = γ ≈ π",
		},
		{
			name: "operators and relations",
			in:   `10 \times 5 \geq 50 \neq 51, and \infty \to \infty`,
			want: "10 × 5 ≥ 50 ≠ 51, and ∞ → ∞",
		},
		{
			name: "square root",
			in:   `\sqrt{2} is about 1.41, and \sqrt{x+1} is not.`,
			want: "√(2) is about 1.41, and √(x+1) is not.",
		},
		{
			name: "cube root",
			in:   `\sqrt[3]{27} = 3`,
			want: "∛(27) = 3",
		},
		{
			name: "multi digit exponent",
			in:   `2^10 = 1024`,
			want: "2¹⁰ = 1024",
		},
		{
			name: "subscripts",
			in:   `x_1 + x_2 = x_{12}`,
			want: "x₁ + x₂ = x₁₂",
		},
		{
			name: "letter subscript is maths",
			in:   `\sum_{i=1}^{n} x_i^2 over v_max`,
			want: "∑ᵢ₌₁ⁿ xᵢ² over v_max",
		},
		{
			name: "word subscript is an identifier",
			in:   "the user_id and the max_value stay as they are",
			want: "the user_id and the max_value stay as they are",
		},
		{
			name: "font wrappers keep their contents",
			in:   `\text{area} = \mathbf{42}`,
			want: "area = 42",
		},
		{
			name: "sizing hints are dropped",
			in:   `\left(\frac{1}{2}\right)`,
			want: "(½)",
		},
		{
			name: "binomial is spelled out",
			in:   `\binom{n}{k}`,
			want: "(n choose k)",
		},
		{
			name: "matrix environment keeps its rows",
			in:   `\begin{pmatrix} a & b \\ c & d \end{pmatrix}`,
			want: " a  b \n c  d ",
		},
		{
			name: "escaped literals",
			in:   `50\% of \$20 is 10\$`,
			want: "50% of $20 is 10$",
		},

		// Text that only looks like maths must survive untouched.
		{
			name: "prices keep their dollar signs",
			in:   "It costs $5 and $10 to ship.",
			want: "It costs $5 and $10 to ship.",
		},
		{
			name: "identifiers keep their underscores",
			in:   "Set user_id and read the max_value first.",
			want: "Set user_id and read the max_value first.",
		},
		{
			name: "windows paths keep their backslashes",
			in:   `Open C:\Users\me\notes.txt`,
			want: `Open C:\Users\me\notes.txt`,
		},
		{
			name: "prose is untouched",
			in:   "Sure! Here is a short answer. 😊",
			want: "Sure! Here is a short answer. 😊",
		},
		{
			name: "unknown commands are left alone",
			in:   `\foo{bar} stays as written`,
			want: `\foo{bar} stays as written`,
		},
		// A stream renders what it has, so a half-arrived command must not
		// be shown; the next update carries the complete token.
		{
			name: "half streamed command is withheld",
			in:   `and \fra`,
			want: "and",
		},
		{
			name: "half streamed delimiter is withheld",
			in:   `value \(`,
			want: "value",
		},
		{
			name: "trailing unknown word is kept",
			in:   `saved to C:\Users`,
			want: `saved to C:\Users`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HumanizeMath(tc.in); got != tc.want {
				t.Errorf("HumanizeMath(%q)\n got  %q\n want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestHumanizeMathIsStable pins idempotency: the sink runs the rewrite on
// every streaming update and again when the answer is finished, so a second
// pass must never change the result.
func TestHumanizeMathIsStable(t *testing.T) {
	inputs := []string{
		`x = \frac{-b \pm \sqrt{b^2-4ac}}{2a}`,
		`\(x^2 + 1\)`,
		`$$\int_0^1 x^2 dx$$`,
		`\alpha \times \beta \leq \infty`,
		"It costs $5 and $10 to ship.",
		"Set user_id and read the max_value first.",
	}

	for _, in := range inputs {
		once := HumanizeMath(in)
		twice := HumanizeMath(once)

		if twice != once {
			t.Errorf("HumanizeMath is not idempotent for %q:\n once %q\n twice %q", in, once, twice)
		}
	}
}

// TestHumanizeMathLeavesOrdinaryTextAlone keeps the fast path honest: an
// answer with no maths must come back byte for byte, so the rewrite can never
// be blamed for a mangled plain reply.
func TestHumanizeMathLeavesOrdinaryTextAlone(t *testing.T) {
	texts := []string{
		"",
		"Hello!",
		"Here are three tips:\n1. Drink water\n2. Sleep well\n3. Stretch",
		"Use the /start command to begin.",
		"Snake_case, kebab-case and camelCase all appear here.",
	}

	for _, text := range texts {
		if got := HumanizeMath(text); got != text {
			t.Errorf("HumanizeMath(%q) = %q, want it unchanged", text, got)
		}
	}
}
