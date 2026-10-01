package coding

import "testing"

func TestAnalyzeDetectsLanguageAndTask(t *testing.T) {
	cases := []struct {
		prompt   string
		wantLang string
		wantTask Task
	}{
		{
			prompt:   "panic: runtime error: invalid memory address\ngoroutine 1 [running]:\npackage main",
			wantLang: "Go",
			wantTask: TaskStackTrace,
		},
		{
			prompt:   "Write unit tests for this Python function:\ndef add(a, b): return a + b",
			wantLang: "Python",
			wantTask: TaskTestGeneration,
		},
		{
			prompt:   "Generate a multi-stage Dockerfile for an Alpine Go service",
			wantLang: "Go",
			wantTask: TaskDockerfile,
		},
		{
			prompt:   "Please perform a code review on this SQL query: SELECT * FROM users WHERE id = 1",
			wantLang: "SQL",
			wantTask: TaskCodeReview,
		},
	}

	for _, tc := range cases {
		got := Analyze(tc.prompt)
		if !got.IsCoding {
			t.Fatalf("Analyze(%q) IsCoding = false, want true", tc.prompt)
		}
		if got.Language != tc.wantLang {
			t.Errorf("Analyze(%q).Language = %q, want %q", tc.prompt, got.Language, tc.wantLang)
		}
		if got.Task != tc.wantTask {
			t.Errorf("Analyze(%q).Task = %q, want %q", tc.prompt, got.Task, tc.wantTask)
		}
	}
}
