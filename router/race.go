package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"openrouter-bot/provider"

	"github.com/sashabaranov/go-openai"
)

// DefaultRaceConcurrency is the default number of models raced in parallel.
const DefaultRaceConcurrency = 3

// MaxRaceConcurrency is the hard ceiling on concurrent race requests so a
// single race cannot flood upstream rate limits.
const MaxRaceConcurrency = 3

// RaceOptions controls how a model race is executed.
type RaceOptions struct {
	// MaxModels is how many candidates to race concurrently (2..3).
	MaxModels int
	// Timeout bounds the entire race.
	Timeout time.Duration
	// Synthesize waits for multiple racers and synthesises the best answer.
	Synthesize bool
}

// RaceResult is the winning (or synthesised) response from a race.
type RaceResult struct {
	Provider     string
	Model        string
	Text         string
	Latency      time.Duration
	Participants []string
	Synthesized  bool
}

// SelectRaceCandidates picks up to maxModels distinct, healthy candidates
// preferring diversity across providers when possible.
func SelectRaceCandidates(chain *provider.Chain, candidates []provider.Candidate, maxModels int) []provider.Candidate {
	if maxModels <= 0 {
		maxModels = DefaultRaceConcurrency
	}
	if maxModels > MaxRaceConcurrency {
		maxModels = MaxRaceConcurrency
	}

	if len(candidates) <= maxModels {
		return append([]provider.Candidate(nil), candidates...)
	}

	healthyProviders := make(map[string]bool)
	if chain != nil {
		for _, info := range chain.Status() {
			if info.Healthy() {
				healthyProviders[strings.ToLower(info.Name)] = true
			}
		}
	}

	selected := make([]provider.Candidate, 0, maxModels)
	seenProvider := make(map[string]bool)
	seenModel := make(map[string]bool)

	// First pass: pick the top candidate from distinct healthy providers.
	for _, cand := range candidates {
		if len(selected) >= maxModels {
			break
		}
		pKey := strings.ToLower(cand.Provider)
		mKey := pKey + "/" + strings.ToLower(cand.Model)
		if len(healthyProviders) > 0 && !healthyProviders[pKey] {
			continue
		}
		if seenProvider[pKey] || seenModel[mKey] {
			continue
		}
		seenProvider[pKey] = true
		seenModel[mKey] = true
		selected = append(selected, cand)
	}

	// Second pass: fill remaining slots with any healthy candidate.
	for _, cand := range candidates {
		if len(selected) >= maxModels {
			break
		}
		pKey := strings.ToLower(cand.Provider)
		mKey := pKey + "/" + strings.ToLower(cand.Model)
		if seenModel[mKey] {
			continue
		}
		seenModel[mKey] = true
		selected = append(selected, cand)
	}

	return selected
}

// Race dispatches req concurrently to 2-3 suitable candidates.
//
// In fast mode (Synthesize == false), the first candidate to return a valid
// non-empty completion wins immediately and cancels all remaining in-flight
// requests.
//
// In synthesis/judge mode (Synthesize == true), it collects up to MaxModels
// responses and synthesises the best answer (or returns the best single answer
// if only one succeeds).
func Race(
	parentCtx context.Context,
	chain *provider.Chain,
	candidates []provider.Candidate,
	req openai.ChatCompletionRequest,
	opts RaceOptions,
) (RaceResult, error) {
	racers := SelectRaceCandidates(chain, candidates, opts.MaxModels)
	if len(racers) == 0 {
		return RaceResult{}, errors.New("no candidates available for race")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}

	raceCtx, cancelRace := context.WithTimeout(parentCtx, timeout)
	defer cancelRace()

	reqCopy := req
	reqCopy.Stream = false

	type outcome struct {
		candidate provider.Candidate
		text      string
		latency   time.Duration
		err       error
	}

	participants := make([]string, 0, len(racers))
	for _, r := range racers {
		participants = append(participants, r.Provider+"/"+r.Model)
	}

	outcomes := make(chan outcome, len(racers))
	var wg sync.WaitGroup

	for _, racer := range racers {
		wg.Add(1)
		go func(cand provider.Candidate) {
			defer wg.Done()
			start := time.Now()
			resp, err := cand.Complete(raceCtx, reqCopy)
			elapsed := time.Since(start)

			if err != nil {
				if chain != nil && !errors.Is(err, context.Canceled) {
					chain.RecordFailure(cand.Provider, err)
				}
				outcomes <- outcome{candidate: cand, latency: elapsed, err: err}
				return
			}

			text := ""
			if len(resp.Choices) > 0 {
				text = strings.TrimSpace(resp.Choices[0].Message.Content)
			}
			if text == "" {
				err = errors.New("empty race completion")
				if chain != nil {
					chain.RecordFailure(cand.Provider, err)
				}
				outcomes <- outcome{candidate: cand, latency: elapsed, err: err}
				return
			}

			if chain != nil {
				chain.RecordSuccessLatency(cand.Provider, cand.Model, elapsed)
			}
			outcomes <- outcome{candidate: cand, text: text, latency: elapsed}
		}(racer)
	}

	if !opts.Synthesize {
		var lastErr error
		for i := 0; i < len(racers); i++ {
			res := <-outcomes
			if res.err == nil && res.text != "" {
				// Cancel remaining slower requests immediately.
				cancelRace()
				return RaceResult{
					Provider:     res.candidate.Provider,
					Model:        res.candidate.Model,
					Text:         res.text,
					Latency:      res.latency,
					Participants: participants,
				}, nil
			}
			lastErr = res.err
		}
		if lastErr == nil {
			lastErr = errors.New("all race candidates failed")
		}
		return RaceResult{}, lastErr
	}

	// Synthesis / Judge mode: collect all valid answers.
	var valid []outcome
	var lastErr error
	for i := 0; i < len(racers); i++ {
		res := <-outcomes
		if res.err == nil && res.text != "" {
			valid = append(valid, res)
		} else if res.err != nil {
			lastErr = res.err
		}
	}

	if len(valid) == 0 {
		if lastErr == nil {
			lastErr = errors.New("all race candidates failed")
		}
		return RaceResult{}, lastErr
	}

	if len(valid) == 1 {
		winner := valid[0]
		return RaceResult{
			Provider:     winner.candidate.Provider,
			Model:        winner.candidate.Model,
			Text:         winner.text,
			Latency:      winner.latency,
			Participants: participants,
		}, nil
	}

	// Use the first valid candidate as judge to synthesise the answers.
	judge := valid[0].candidate
	var sb strings.Builder
	sb.WriteString("Synthesize the strongest, most accurate final answer from these candidate model responses. Return only the final answer in plain text.\n\n")
	for i, v := range valid {
		sb.WriteString(fmt.Sprintf("--- Candidate %d (%s/%s) ---\n%s\n\n", i+1, v.candidate.Provider, v.candidate.Model, v.text))
	}

	judgeCtx, cancelJudge := context.WithTimeout(parentCtx, 25*time.Second)
	defer cancelJudge()

	judgeReq := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are an expert answer synthesizer. Combine the best parts of the candidate answers concisely."},
			{Role: openai.ChatMessageRoleUser, Content: sb.String()},
		},
		Temperature: 0.3,
		MaxTokens:   req.MaxTokens,
	}

	if synthResp, err := judge.Complete(judgeCtx, judgeReq); err == nil && len(synthResp.Choices) > 0 {
		if synthText := strings.TrimSpace(synthResp.Choices[0].Message.Content); synthText != "" {
			return RaceResult{
				Provider:     judge.Provider,
				Model:        judge.Model,
				Text:         synthText,
				Latency:      valid[0].latency,
				Participants: participants,
				Synthesized:  true,
			}, nil
		}
	}

	// Fallback to the fastest valid answer if synthesis fails.
	winner := valid[0]
	return RaceResult{
		Provider:     winner.candidate.Provider,
		Model:        winner.candidate.Model,
		Text:         winner.text,
		Latency:      winner.latency,
		Participants: participants,
	}, nil
}
