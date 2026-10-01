package user

import (
	"strings"

	"openrouter-bot/provider"
)

// Settings are the per user preferences that survive a restart. They live in
// the same JSON document as the spend counters, so a user has exactly one
// file and one write path.
type Settings struct {
	// Auto makes the bot choose the model from the usage profile. It stays
	// true until the user picks something explicitly.
	Auto bool `json:"auto"`
	// Provider pins a provider ("groq", "gemini", …). Empty means any.
	Provider string `json:"provider,omitempty"`
	// Model pins an exact model id. Empty means any model.
	Model string `json:"model,omitempty"`
	// ShowFooter adds "answered by provider/model" under each answer.
	ShowFooter bool `json:"show_footer"`
	// Streaming edits the message while tokens arrive. Off means the answer
	// appears once it is complete (nicer on very slow connections).
	Streaming bool `json:"streaming"`
	// Favourites are bookmarked models, shown first in the picker.
	Favourites []string `json:"favourites,omitempty"`
	// Downvotes counts negative feedback per model. Auto routing reads it so
	// a model the user rejected is not picked again.
	Downvotes map[string]int `json:"downvotes,omitempty"`
	// HintsShown counts the "this model may suit you better" nudges.
	HintsShown int `json:"hints_shown,omitempty"`
	// initialised is set once the defaults above have been written, so a file
	// from an older version gets sensible values without wiping choices.
	Initialised bool `json:"settings_initialised,omitempty"`
}

// DefaultSettings are used for a user whose file predates preferences.
func DefaultSettings() Settings {
	return Settings{
		Auto:       true,
		ShowFooter: true,
		Streaming:  true,
		Downvotes:  map[string]int{},
	}
}

// Settings returns a copy of the stored preferences with defaults applied.
func (ut *UsageTracker) Settings() Settings {
	ut.UsageMu.Lock()
	defer ut.UsageMu.Unlock()

	settings := Settings{}
	if ut.Usage != nil {
		settings = ut.Usage.Settings
	}

	if !settings.Initialised {
		def := DefaultSettings()
		def.Favourites = settings.Favourites
		def.Provider = settings.Provider
		def.Model = settings.Model
		def.Downvotes = settings.Downvotes
		// A model or provider chosen in a previous version wins over auto.
		def.Auto = settings.Provider == "" && settings.Model == ""
		settings = def
	}

	if settings.Downvotes == nil {
		settings.Downvotes = map[string]int{}
	}

	return settings
}

// updateSettings applies a change under the usage lock and persists it.
func (ut *UsageTracker) updateSettings(mutate func(*Settings)) {
	ut.UsageMu.Lock()

	if ut.Usage == nil {
		ut.Usage = newUserUsage(ut.UserName)
	}

	settings := ut.Usage.Settings
	if !settings.Initialised {
		def := DefaultSettings()
		def.Provider = settings.Provider
		def.Model = settings.Model
		def.Downvotes = settings.Downvotes
		def.Favourites = settings.Favourites
		def.Auto = settings.Provider == "" && settings.Model == ""
		settings = def
	}
	if settings.Downvotes == nil {
		settings.Downvotes = map[string]int{}
	}

	mutate(&settings)
	settings.Initialised = true
	ut.Usage.Settings = settings

	ut.UsageMu.Unlock()

	if err := ut.saveUsage(); err != nil {
		logSaveFailure("settings", ut.UserID, err)
	}
}

// SetAuto switches between automatic model selection and the pinned choice.
func (ut *UsageTracker) SetAuto(auto bool) {
	ut.updateSettings(func(settings *Settings) {
		settings.Auto = auto
		if auto {
			// Auto means "no pin": keeping a pinned model around would make
			// the next explicit choice look like it was still in force.
			settings.Model = ""
		}
	})
}

// SetProvider pins a provider while keeping automatic model choice inside it.
func (ut *UsageTracker) SetProvider(name string) {
	ut.updateSettings(func(settings *Settings) {
		settings.Provider = strings.TrimSpace(name)
		settings.Model = ""
		settings.Auto = settings.Provider == ""
	})
}

// SetModel pins an exact model and the provider that serves it.
func (ut *UsageTracker) SetModel(model, providerName string) {
	ut.updateSettings(func(settings *Settings) {
		settings.Model = strings.TrimSpace(model)
		settings.Provider = strings.TrimSpace(providerName)
		settings.Auto = false
	})
}

// ResetPreference goes back to automatic routing.
func (ut *UsageTracker) ResetPreference() {
	ut.updateSettings(func(settings *Settings) {
		settings.Auto = true
		settings.Model = ""
		settings.Provider = ""
	})
}

// SetShowFooter toggles the "answered by" line under each answer.
func (ut *UsageTracker) SetShowFooter(show bool) {
	ut.updateSettings(func(settings *Settings) {
		settings.ShowFooter = show
	})
}

// SetStreaming toggles live message editing.
func (ut *UsageTracker) SetStreaming(streaming bool) {
	ut.updateSettings(func(settings *Settings) {
		settings.Streaming = streaming
	})
}

// ToggleFooter flips the footer flag and returns the new value.
func (ut *UsageTracker) ToggleFooter() bool {
	settings := ut.Settings()
	ut.SetShowFooter(!settings.ShowFooter)

	return !settings.ShowFooter
}

// ToggleStreaming flips live editing and returns the new value.
func (ut *UsageTracker) ToggleStreaming() bool {
	settings := ut.Settings()
	ut.SetStreaming(!settings.Streaming)

	return !settings.Streaming
}

// ToggleFavourite adds or removes a model from the user's bookmarks.
func (ut *UsageTracker) ToggleFavourite(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}

	added := false

	ut.updateSettings(func(settings *Settings) {
		for i, favourite := range settings.Favourites {
			if strings.EqualFold(favourite, model) {
				settings.Favourites = append(settings.Favourites[:i], settings.Favourites[i+1:]...)
				return
			}
		}

		settings.Favourites = append(settings.Favourites, model)
		added = true
	})

	return added
}

// IsFavourite reports whether a model is bookmarked.
func (ut *UsageTracker) IsFavourite(model string) bool {
	for _, favourite := range ut.Settings().Favourites {
		if strings.EqualFold(favourite, model) {
			return true
		}
	}

	return false
}

// Feedback records a thumbs up or down for the model that produced an answer.
// A downvote is remembered per model and steers the automatic routing away
// from it.
func (ut *UsageTracker) Feedback(model string, positive bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}

	ut.updateSettings(func(settings *Settings) {
		if positive {
			// A model that was just liked should not stay on the naughty
			// list from an earlier disappointment.
			if count := settings.Downvotes[model]; count > 0 {
				if count == 1 {
					delete(settings.Downvotes, model)
				} else {
					settings.Downvotes[model] = count - 1
				}
			}
			return
		}

		settings.Downvotes[model]++
	})
}

// HintsShown returns how many model suggestions the user has seen.
func (ut *UsageTracker) HintsShown() int {
	return ut.Settings().HintsShown
}

// MarkHintShown records that a suggestion was delivered.
func (ut *UsageTracker) MarkHintShown() {
	ut.updateSettings(func(settings *Settings) {
		settings.HintsShown++
	})
}

// Preference converts the stored settings into a routing preference.
func (ut *UsageTracker) Preference() provider.Preference {
	settings := ut.Settings()

	return provider.Preference{
		Provider: settings.Provider,
		Model:    settings.Model,
		Auto:     settings.Auto || settings.Model == "",
	}
}

// DislikedModels exposes the downvote counters for the routing profile.
func (ut *UsageTracker) DislikedModels() map[string]int {
	settings := ut.Settings()
	if len(settings.Downvotes) == 0 {
		return nil
	}

	return settings.Downvotes
}

// SettingsSummary is a one line description of the user's current choice, for
// the settings screen.
func (ut *UsageTracker) SettingsSummary() string {
	preference := ut.Preference()

	switch {
	case !preference.Auto && preference.Model != "":
		return preference.Model
	case preference.Provider != "":
		return "auto · " + preference.Provider
	default:
		return "auto · best for your usage"
	}
}
