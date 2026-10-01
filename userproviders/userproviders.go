// Package userproviders stores the AI providers a Telegram user brings
// themselves, with their API keys encrypted at rest.
//
// Every record belongs to exactly one Telegram user id: that id is the storage
// key and every method takes it, so one user can never read, use, change or
// delete another user's provider. The key itself is sealed with AES-256-GCM
// under a master key that only exists as a server environment variable, and it
// is decrypted for the single request that needs it.
package userproviders

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"openrouter-bot/internal/security"
	"openrouter-bot/provider"
	"openrouter-bot/storage"
)

// Namespace is where per-user provider documents live inside the shared store.
const Namespace = "user_providers"

// MaxPerUser bounds how many providers one user may register.
const MaxPerUser = 10

// Provider is one user owned backend. The API key is only ever stored sealed:
// EncryptedAPIKey is what sits in the database, and it is never logged,
// rendered or sent to a model.
type Provider struct {
	TelegramUserID  int64     `json:"telegram_user_id"`
	ID              string    `json:"id"`
	Name            string    `json:"provider_name"`
	Model           string    `json:"model"`
	BaseURL         string    `json:"base_url"`
	EncryptedAPIKey string    `json:"encrypted_api_key"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// document is the per-user JSON document.
type document struct {
	Providers []Provider `json:"providers"`
}

// Store persists user providers through the bot's existing storage layer.
type Store struct {
	store storage.Store
	// masterKey seals and opens API keys. It comes from the server
	// environment only and is never written anywhere.
	masterKey string
}

// NewStore builds the store. A missing master key leaves it disabled: user
// providers are refused rather than kept in plaintext.
func NewStore(store storage.Store, masterKey string) *Store {
	return &Store{store: store, masterKey: strings.TrimSpace(masterKey)}
}

// Enabled reports whether user owned providers can be used at all.
func (s *Store) Enabled() bool {
	return s != nil && s.store != nil && s.masterKey != ""
}

// List returns the providers of one Telegram user, in insertion order.
func (s *Store) List(ctx context.Context, telegramUserID int64) ([]Provider, error) {
	if !s.Enabled() {
		return nil, security.ErrNoMasterKey
	}

	data, err := s.store.Load(ctx, Namespace, userKey(telegramUserID))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}

	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("reading stored providers: %w", err)
	}

	out := make([]Provider, 0, len(doc.Providers))
	for _, record := range doc.Providers {
		// Ownership is enforced here and not only at the storage key: a
		// record that claims another user is dropped, never served.
		if record.TelegramUserID != telegramUserID {
			continue
		}

		out = append(out, record)
	}

	return out, nil
}

// Get returns one provider of one user. The boolean is false when the user
// does not own that id.
func (s *Store) Get(ctx context.Context, telegramUserID int64, id string) (Provider, bool, error) {
	list, err := s.List(ctx, telegramUserID)
	if err != nil {
		return Provider{}, false, err
	}

	want := strings.TrimSpace(id)
	for _, record := range list {
		if strings.EqualFold(record.ID, want) {
			return record, true, nil
		}
	}

	return Provider{}, false, nil
}

// Add registers a provider for one user, sealing the key before it is stored.
func (s *Store) Add(
	ctx context.Context,
	telegramUserID int64,
	name, model, baseURL, apiKey string,
) (Provider, error) {
	if !s.Enabled() {
		return Provider{}, security.ErrNoMasterKey
	}

	name = strings.TrimSpace(name)
	baseURL = strings.TrimSpace(baseURL)
	apiKey = strings.TrimSpace(apiKey)

	if name == "" || len([]rune(name)) > 40 {
		return Provider{}, errors.New("provider name must be 1-40 characters")
	}
	if baseURL == "" {
		return Provider{}, errors.New("base url is required")
	}
	if apiKey == "" {
		return Provider{}, errors.New("api key is required")
	}

	list, err := s.List(ctx, telegramUserID)
	if err != nil {
		return Provider{}, err
	}
	if len(list) >= MaxPerUser {
		return Provider{}, fmt.Errorf("limit of %d providers reached", MaxPerUser)
	}
	for _, existing := range list {
		if strings.EqualFold(existing.Name, name) {
			return Provider{}, fmt.Errorf("a provider named %q already exists", existing.Name)
		}
	}

	sealed, err := security.EncryptSecret(apiKey, s.masterKey)
	if err != nil {
		return Provider{}, err
	}

	now := time.Now().UTC()
	record := Provider{
		TelegramUserID:  telegramUserID,
		ID:              newID(),
		Name:            name,
		Model:           strings.TrimSpace(model),
		BaseURL:         baseURL,
		EncryptedAPIKey: sealed,
		Enabled:         true,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := s.save(ctx, telegramUserID, append(list, record)); err != nil {
		return Provider{}, err
	}

	return record, nil
}

// SetModel stores the model a user picked for one of their providers.
func (s *Store) SetModel(ctx context.Context, telegramUserID int64, id, model string) error {
	list, err := s.List(ctx, telegramUserID)
	if err != nil {
		return err
	}

	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model must not be empty")
	}

	want := strings.TrimSpace(id)
	for i, record := range list {
		if !strings.EqualFold(record.ID, want) {
			continue
		}

		list[i].Model = model
		list[i].UpdatedAt = time.Now().UTC()

		return s.save(ctx, telegramUserID, list)
	}

	return fmt.Errorf("provider %q not found", want)
}

// Remove deletes one provider of one user. It reports false when the user does
// not own that id, which is also the only way another user's record is
// protected from this call.
func (s *Store) Remove(ctx context.Context, telegramUserID int64, id string) (bool, error) {
	list, err := s.List(ctx, telegramUserID)
	if err != nil {
		return false, err
	}

	want := strings.TrimSpace(id)
	kept := make([]Provider, 0, len(list))
	removed := false

	for _, record := range list {
		if strings.EqualFold(record.ID, want) {
			removed = true
			continue
		}

		kept = append(kept, record)
	}

	if !removed {
		return false, nil
	}

	return true, s.save(ctx, telegramUserID, kept)
}

// APIKey opens the sealed key of one of a user's providers. The plaintext is
// handed to the caller for one request and is never stored, logged or
// rendered.
func (s *Store) APIKey(ctx context.Context, telegramUserID int64, id string) (string, bool, error) {
	record, ok, err := s.Get(ctx, telegramUserID, id)
	if err != nil || !ok {
		return "", false, err
	}

	key, err := record.APIKey(s.masterKey)
	if err != nil {
		return "", false, err
	}

	return key, true, nil
}

// ProviderConfig resolves one of a user's providers by name and returns a
// ready to use provider config with the key decrypted. This is the only place
// a user key becomes usable, and it happens right before the request that
// needs it.
func (s *Store) ProviderConfig(
	ctx context.Context,
	telegramUserID int64,
	name string,
) (provider.Config, bool, error) {
	list, err := s.List(ctx, telegramUserID)
	if err != nil {
		return provider.Config{}, false, err
	}

	want := strings.TrimSpace(name)
	for _, record := range list {
		if !record.Enabled || !strings.EqualFold(record.Name, want) {
			continue
		}
		if strings.TrimSpace(record.Model) == "" {
			continue
		}

		cfg, err := record.Config(s.masterKey)
		if err != nil {
			return provider.Config{}, false, err
		}

		return cfg, true, nil
	}

	return provider.Config{}, false, nil
}

// APIKey opens this record's sealed key. The caller must drop the result as
// soon as the request that needed it is done.
func (p Provider) APIKey(masterKey string) (string, error) {
	if strings.TrimSpace(p.EncryptedAPIKey) == "" {
		return "", errors.New("provider has no stored api key")
	}

	return security.DecryptSecret(p.EncryptedAPIKey, masterKey)
}

// Config turns the record into a provider the chain can use, decrypting the
// key for that one request.
func (p Provider) Config(masterKey string) (provider.Config, error) {
	key, err := p.APIKey(masterKey)
	if err != nil {
		return provider.Config{}, err
	}

	return provider.Config{
		Name:        p.Name,
		BaseURL:     p.BaseURL,
		APIKey:      key,
		Models:      []string{p.Model},
		RequiresKey: true,
	}, nil
}

func (s *Store) save(ctx context.Context, telegramUserID int64, list []Provider) error {
	data, err := json.Marshal(document{Providers: list})
	if err != nil {
		return fmt.Errorf("encoding providers: %w", err)
	}

	return s.store.Save(ctx, Namespace, userKey(telegramUserID), data)
}

func userKey(telegramUserID int64) string {
	return strconv.FormatInt(telegramUserID, 10)
}

// newID returns a short random identifier. It is not a secret: it only
// addresses one record inside one user's document.
func newID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}

	return hex.EncodeToString(raw)
}
