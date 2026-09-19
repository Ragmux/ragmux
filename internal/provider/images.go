package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// contentPart is one OpenAI content part. Every adapter used to re-parse the
// raw content itself, which is how the three of them ended up disagreeing
// about parts without a type; they now all read this.
type contentPart struct {
	// Type is "text" or "image_url"; a part without a type is text, the way
	// OpenAI reads it.
	Type  string
	Text  string
	Image imageRef
	// CacheControl is the raw cache_control the client attached to the part.
	// Only Anthropic has anything to do with it; it is carried here so the
	// adapter that can use it does not have to parse the content again.
	CacheControl json.RawMessage
}

// imageRef is an image part with exactly one of Base64 (a data: URL, already
// split into media type and payload) or URL set.
type imageRef struct {
	MediaType string
	Base64    string
	URL       string
}

// parseContent normalises a message's content — a bare string or an array of
// parts — into content parts. Parts of a type no adapter can carry
// (input_audio, file, ...) are dropped rather than rejected: the model just
// does not see them, which beats failing a conversation over one part.
func parseContent(raw json.RawMessage, accepts map[string]bool) ([]contentPart, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []contentPart{{Type: "text", Text: s}}, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "unsupported message content"}
	}
	out := make([]contentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text", "":
			out = append(out, contentPart{Type: "text", Text: p.Text, CacheControl: p.CacheControl})
		case "image_url":
			if p.ImageURL.URL == "" {
				// Nothing to send and nothing to complain about, the way a
				// part of a type no adapter can carry is dropped.
				continue
			}
			ref, err := parseImageURL(p.ImageURL.URL, accepts)
			if err != nil {
				return nil, err
			}
			out = append(out, contentPart{Type: "image_url", Image: ref, CacheControl: p.CacheControl})
		}
	}
	return out, nil
}

// Image inlining policy, one constant per adapter so moving a provider from
// one side to the other is a single line:
//   - Ollama accepts nothing but inline base64.
//   - Gemini's fileData takes Files API and gs:// URIs only, so every other
//     URL has to arrive as bytes.
//   - Anthropic fetches a url image source itself, which is cheaper and puts
//     fewer bytes on our wire. A Bedrock-style gateway in front of Anthropic
//     that has no url source only has to flip this.
const (
	anthropicInlineImages = false
	geminiInlineImages    = true
	ollamaInlineImages    = true
)

// imageMediaTypes is what each upstream documents itself as accepting, for a
// fetched image and an inline data: one alike. For a fetched one the URL's
// extension is never consulted; the response's own content type decides,
// because a URL can be spelled to claim anything.
//
// The lists genuinely differ, so one shared list is wrong in both directions:
// it refused the image/heic and image/heif Gemini takes, breaking requests
// that worked before anything was checked, and it would have promised
// Anthropic a format its API rejects. Each adapter asks for its own.
var imageMediaTypes = map[string]map[string]bool{
	"gemini": {"image/png": true, "image/jpeg": true, "image/webp": true,
		"image/heic": true, "image/heif": true},
	"anthropic": {"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true},
	"ollama":    {"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true},
}

// imageMediaTypesDefault is what a caller with no adapter behind it gets: the
// four formats every vision provider here takes.
var imageMediaTypesDefault = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

func imageMediaTypesFor(providerType string) map[string]bool {
	if types, ok := imageMediaTypes[providerType]; ok {
		return types
	}
	return imageMediaTypesDefault
}

// imageTypesInMessage lists an accepted set for an error message, shortened
// the way a caller writes them ("png, jpeg, ...").
func imageTypesInMessage(accepts map[string]bool) string {
	out := make([]string, 0, len(accepts))
	for t := range accepts {
		out = append(out, strings.TrimPrefix(t, "image/"))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

const (
	// defaultImageMaxConcurrent is how many image fetches the process runs at
	// once when nothing says otherwise. It is well above what one chat
	// request can spend -- an adapter pulls its images one at a time -- so
	// the ceiling binds across requests rather than throttling a single one.
	defaultImageMaxConcurrent = 16
	// defaultImageQueueWait bounds the wait for a slot. It is deliberately a
	// small fraction of Timeout: queueing is not progress, and a client told
	// when to come back is better served than one held to the end of a
	// download it is not getting.
	defaultImageQueueWait = 2 * time.Second
)

// ImageFetcher resolves remote image URLs into base64 for adapters whose
// upstream cannot fetch a URL itself.
//
// It must be given the same netguard-hardened client the adapters use. An
// image URL comes from the API client, so it is exactly the kind of input the
// dialer's private-address filter and the redirect policy exist for; a second
// outbound path would be a second thing to keep safe.
type ImageFetcher struct {
	// Client is required.
	Client *http.Client
	// MaxBytes caps one image (default 8 MiB).
	MaxBytes int64
	// Timeout bounds one image fetch (default 10s).
	Timeout time.Duration
	// MaxPerRequest caps the images one chat request may pull (default 8).
	MaxPerRequest int
	// MaxConcurrent caps the image fetches in flight across the whole
	// process (default 16).
	//
	// MaxPerRequest bounds one chat request and bounds nothing at all when a
	// key holder sends many at once: every one of them turns into GETs
	// against whatever host it names, from the gateway's own address. This is
	// the ceiling that makes the gateway a poor amplifier — and the reason
	// the image transport also carries MaxConnsPerHost.
	MaxConcurrent int
	// QueueWait bounds the wait for one of those slots, separately from
	// Timeout: a fetch that spent its download budget in the queue would
	// blame the image host for the gateway being busy. It defaults to 2s, or
	// to Timeout when that is shorter, since queueing past the budget the
	// download itself gets is never worth it.
	QueueWait time.Duration
	// Cache is optional; nil fetches every time.
	Cache  *imageCache
	Logger *slog.Logger

	semOnce sync.Once
	sem     chan struct{}

	mu sync.Mutex
	// inflight is slots held per tenant; an entry is deleted at zero, so an
	// idle tenant leaves nothing behind.
	inflight map[string]int
	// queued is the same tenant's acquirers already promised a slot by the
	// share and waiting for one. It counts towards the share alongside
	// inflight, because inflight only rises once a slot is in hand: a burst
	// of acquirers from one tenant all read the same pre-burst number and all
	// queued as entitled, so the tenant could pass its share while another
	// was queueing -- the share held eventually rather than throughout.
	queued map[string]int
	// waiting counts acquirers queued for a slot they are entitled to, which
	// is what tells a tenant already over its share to leave the next freed
	// slot alone.
	waiting int
	// wake is closed and replaced on every release, so an acquirer parked
	// outside its share re-checks without polling.
	wake chan struct{}
}

func (f *ImageFetcher) init() {
	f.semOnce.Do(func() {
		f.sem = make(chan struct{}, f.maxConcurrent())
		f.inflight = map[string]int{}
		f.queued = map[string]int{}
		f.wake = make(chan struct{})
	})
}

// tenantShare is how many slots one tenant may hold before it has to give way
// to another. It is a floor under everyone else's access, not a ceiling on
// this tenant: capping the process alone let a few requests aimed at one slow
// image host fill every slot and leave an unrelated project to wait out its
// whole timeout, but capping each tenant hard was worse in the common case --
// a single-project install would have reached only half of
// IMAGE_FETCH_MAX_CONCURRENT, making the setting mean half of what it says.
// So the share binds only while someone else is queueing.
func (f *ImageFetcher) tenantShare() int {
	if n := f.maxConcurrent() / 2; n > 0 {
		return n
	}
	return 1
}

// acquire takes a process-wide slot for tenant and returns its release. The
// wait has its own budget, so the download timeout starts only once the slot
// is in hand.
//
// Within its share a tenant queues for a slot like anyone else. Past its
// share it may still use spare capacity -- an idle gateway belongs to
// whoever is on it -- but only while nobody entitled is queueing, and only
// without parking on the semaphore, since a parked sender would be served
// ahead of the next entitled arrival.
func (f *ImageFetcher) acquire(ctx context.Context, tenant string) (func(), error) {
	f.init()
	wait, cancel := context.WithTimeout(ctx, f.queueWait())
	defer cancel()

	share := f.tenantShare()
	for {
		f.mu.Lock()
		if f.inflight[tenant]+f.queued[tenant] < share {
			f.waiting++
			f.queued[tenant]++
			f.mu.Unlock()
			select {
			case f.sem <- struct{}{}:
				f.mu.Lock()
				f.waiting--
				f.dequeue(tenant)
				f.inflight[tenant]++
				// The tenant's own total did not move, so only the process
				// queue emptying can newly permit anybody.
				f.wakeIfQueueEmpty()
				f.mu.Unlock()
				return f.release(tenant), nil
			case <-wait.Done():
				f.mu.Lock()
				f.waiting--
				f.dequeue(tenant)
				// Unconditional, unlike the arm above, and it has to stay
				// that way: this one lowers the tenant's total without
				// raising inflight, so a sibling parked outside the share may
				// have just come back inside it. wakeIfQueueEmpty would say
				// nothing while another tenant is still queued, and that
				// sibling would sleep out its whole wait next to a claim it
				// had just regained.
				//
				// No test holds this. Reaching it needs a same-tenant
				// acquirer to time out while another tenant is still
				// queueing, which is not an interleaving a test can force,
				// and narrowing this to wakeIfQueueEmpty passes the whole
				// suite. It is here by argument, not by coverage.
				f.broadcast()
				f.mu.Unlock()
				return nil, f.queueError(ctx)
			}
		}
		if f.waiting == 0 {
			select {
			case f.sem <- struct{}{}:
				f.inflight[tenant]++
				f.mu.Unlock()
				return f.release(tenant), nil
			default:
			}
		}
		wake := f.wake
		f.mu.Unlock()
		select {
		case <-wake:
		case <-wait.Done():
			return nil, f.queueError(ctx)
		}
	}
}

// dequeue drops one of the tenant's promised slots. The caller holds f.mu.
func (f *ImageFetcher) dequeue(tenant string) {
	if n := f.queued[tenant] - 1; n > 0 {
		f.queued[tenant] = n
	} else {
		delete(f.queued, tenant)
	}
}

// release gives the slot back and wakes whoever was held outside its share.
// The semaphore is drained first so a woken acquirer finds the capacity.
func (f *ImageFetcher) release(tenant string) func() {
	return func() {
		<-f.sem
		f.mu.Lock()
		if n := f.inflight[tenant] - 1; n > 0 {
			f.inflight[tenant] = n
		} else {
			delete(f.inflight, tenant)
		}
		f.broadcast()
		f.mu.Unlock()
	}
}

// wakeIfQueueEmpty wakes the acquirers parked outside their share once the
// last entitled one has left the queue. Waking only on release was not
// enough: an acquirer over its share parks because somebody entitled is
// queued, and that queue empties when the waiter *takes* a slot, which frees
// nothing and so sent no wake. It then slept out the whole queue wait beside
// capacity it was allowed to use -- with four concurrent fetches from one
// tenant, the fourth reliably gave up on an idle gateway.
//
// The caller holds f.mu.
func (f *ImageFetcher) wakeIfQueueEmpty() {
	if f.waiting == 0 {
		f.broadcast()
	}
}

// broadcast releases everyone parked on wake so they re-read the state. The
// caller holds f.mu.
func (f *ImageFetcher) broadcast() {
	close(f.wake)
	f.wake = make(chan struct{})
}

// queueError separates the gateway running out from the caller going away. A
// cancelled request context is a client that hung up, and that is what the
// caller gets back: the gateway's own handlers check ctx.Err() before they
// classify a provider error, so wrapping a disconnect in one here would only
// have travelled to whatever else reads these errors.
func (f *ImageFetcher) queueError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	// Not the client's mistake and not the image host's, so neither a 400 nor
	// a relayed upstream failure. 429 rather than 503 because nothing is
	// broken: a 5xx would report a fault, count against availability, and say
	// the gateway is unwell when it is merely full.
	//
	// Retry-After is the fetch timeout, not the queue wait. A slot frees when
	// a download finishes, and a download may take the whole timeout, so
	// naming the queue wait sent a client that honours the header -- which
	// the official SDKs do, for a 429 as much as for a 5xx -- back into the
	// same full queue several times over one round of slow fetches.
	retry := imageCeilSeconds(f.timeout())
	return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_exceeded",
		Code: "image_fetch_saturated", RetryAfter: retry,
		Message: fmt.Sprintf("the gateway is fetching as many images as it may at once; retry after %d seconds", retry)}
}

func imageCeilSeconds(d time.Duration) int {
	if n := int((d + time.Second - 1) / time.Second); n > 0 {
		return n
	}
	return 1
}

// Fetch downloads one image and returns its media type and base64 payload.
// Everything a client can get wrong is a 400 naming only the host: the full
// URL may carry a signed query string, and it is the client's own string
// anyway. Transport and policy failures go through transportError so a
// private-address image URL reads like a private base URL.
func (f *ImageFetcher) Fetch(ctx context.Context, rawURL, tenant string, accepts map[string]bool) (string, string, error) {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", imageError("image URLs must be http or https")
	}
	if mt, data, ok := f.Cache.get(rawURL); ok {
		// The cache is keyed on the URL alone and the adapters do not accept
		// the same formats, so a hit is checked again rather than trusted:
		// an image/heic stored for Gemini must not be handed to Anthropic.
		// Nothing was requested on this path, so nothing "returned" anything.
		if !accepts[mt] {
			return "", "", imageError(fmt.Sprintf("the image at %s is %q; %s are accepted",
				u.Host, mt, imageTypesInMessage(accepts)))
		}
		return mt, data, nil
	}
	// Refuse rather than fall back to http.DefaultClient. This is the one
	// place the gateway follows a URL a client chose, and it is only safe
	// because it goes out over the same guarded transport as a provider call:
	// an unguarded default client here would reach loopback and private
	// addresses, which is exactly what netguard exists to prevent.
	if f.Client == nil {
		return "", "", imageError("remote image fetching is not configured")
	}
	// The fetcher has no connection of its own; ProviderType and Logger are
	// what transportError needs to log and classify the failure.
	cfg := Config{ProviderType: "image", Logger: f.Logger}
	release, err := f.acquire(ctx, tenant)
	if err != nil {
		return "", "", err
	}
	defer release()
	// The download's budget starts once the slot is in hand. Starting it
	// before the queue charged the wait to the host: the same saturation came
	// back as a 429 or, when the wait ate most of the budget, as a transport
	// timeout relayed as a 502 that blamed the image host.
	ctx, cancel := context.WithTimeout(ctx, f.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", imageError("image URL is not a valid request target")
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "ragmux/1.0")
	resp, err := f.Client.Do(req)
	if err != nil {
		return "", "", transportError(cfg, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 400 rather than 502: the client picked the host, not the operator.
		return "", "", imageError(fmt.Sprintf("image URL returned %d", resp.StatusCode))
	}
	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if !accepts[mediaType] {
		return "", "", imageError(fmt.Sprintf("image URL at %s returned content type %q; %s are accepted",
			u.Host, mediaType, imageTypesInMessage(accepts)))
	}
	if resp.ContentLength > f.maxBytes() {
		return "", "", imageError(f.tooLarge(u.Host))
	}
	// Read one byte past the limit: an absent or lying Content-Length must not
	// be able to spend more memory than the cap allows.
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes()+1))
	if err != nil {
		return "", "", transportError(cfg, rawURL, err)
	}
	if int64(len(body)) > f.maxBytes() {
		return "", "", imageError(f.tooLarge(u.Host))
	}
	data := base64.StdEncoding.EncodeToString(body)
	if !strings.Contains(strings.ToLower(resp.Header.Get("Cache-Control")), "no-store") {
		f.Cache.put(rawURL, mediaType, data)
	}
	return mediaType, data, nil
}

func (f *ImageFetcher) tooLarge(host string) string {
	return fmt.Sprintf("image at %s is larger than the %d MiB limit", host, f.maxBytes()>>20)
}

func (f *ImageFetcher) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return 8 << 20
}

func (f *ImageFetcher) timeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return 10 * time.Second
}

func (f *ImageFetcher) maxPerRequest() int {
	if f.MaxPerRequest > 0 {
		return f.MaxPerRequest
	}
	return 8
}

func (f *ImageFetcher) maxConcurrent() int {
	if f.MaxConcurrent > 0 {
		return f.MaxConcurrent
	}
	return defaultImageMaxConcurrent
}

func (f *ImageFetcher) queueWait() time.Duration {
	if f.QueueWait > 0 {
		return f.QueueWait
	}
	if t := f.timeout(); t < defaultImageQueueWait {
		return t
	}
	return defaultImageQueueWait
}

// imageTenantKey carries the identity the fetch ceiling shares slots between.
type imageTenantKey struct{}

// WithImageTenant tags a request context with who is asking, so one project
// cannot hold more than its share of the image fetch slots. An untagged
// context is one anonymous tenant, which is right for the paths that are not
// serving an API client.
func WithImageTenant(ctx context.Context, tenant string) context.Context {
	if tenant == "" {
		return ctx
	}
	return context.WithValue(ctx, imageTenantKey{}, tenant)
}

func imageTenantFrom(ctx context.Context) string {
	t, _ := ctx.Value(imageTenantKey{}).(string)
	return t
}

// imageBudget is one chat request's share of the fetcher: images are pulled
// one at a time and only so many of them, so a single client request cannot
// fan out into a burst of outbound connections.
type imageBudget struct {
	ctx context.Context
	cfg Config
	// accepts is the adapter's own media type whitelist; the fetcher has no
	// opinion of its own because the upstreams genuinely differ.
	accepts map[string]bool
	taken   int
}

func (c Config) imageBudget(ctx context.Context) *imageBudget {
	return &imageBudget{ctx: ctx, cfg: c, accepts: imageMediaTypesFor(c.ProviderType)}
}

// accepted is the whitelist to parse inline images against; a nil budget
// belongs to a caller with no adapter behind it.
func (b *imageBudget) accepted() map[string]bool {
	if b == nil || b.accepts == nil {
		return imageMediaTypesDefault
	}
	return b.accepts
}

// inline resolves a remote image into base64. A reference that is already
// inline, or a gateway with image fetching switched off, is returned
// unchanged — the adapter then applies whatever it did before.
func (b *imageBudget) inline(ref imageRef) (imageRef, error) {
	if b == nil || b.cfg.Images == nil || ref.URL == "" {
		return ref, nil
	}
	if b.taken >= b.cfg.Images.maxPerRequest() {
		return ref, imageError(fmt.Sprintf("a chat request may fetch at most %d remote images", b.cfg.Images.maxPerRequest()))
	}
	b.taken++
	mediaType, data, err := b.cfg.Images.Fetch(b.ctx, ref.URL, imageTenantFrom(b.ctx), b.accepted())
	if err != nil {
		return ref, err
	}
	return imageRef{MediaType: mediaType, Base64: data}, nil
}

func imageError(msg string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: msg}
}

// parseImageURL splits a data: URL into its media type and payload, and keeps
// anything else as a URL for the adapter's own policy to resolve.
//
// A data: URL is held to the same whitelist a fetched image is, and for the
// same reason: it is the client's own string. Passing it through unchecked
// sent "data:text/html;base64,…" upstream as an image, and a "data:,payload"
// with neither a type nor base64 encoding as well, each coming back as a
// provider 400 that named nothing the caller could act on. The message never
// echoes the payload.
func parseImageURL(u string, accepts map[string]bool) (imageRef, error) {
	// RFC 3986 makes a scheme name case-insensitive, and "DATA:" used to fall
	// through to the remote path, where none of this ran.
	if len(u) < len("data:") || !strings.EqualFold(u[:len("data:")], "data:") {
		return imageRef{URL: u}, nil
	}
	meta, payload, ok := strings.Cut(u[len("data:"):], ",")
	if !ok || !imageDataIsBase64(meta) {
		return imageRef{}, imageError(
			"an inline image must be a base64 data: URL, as in data:image/png;base64,<payload>")
	}
	mediaType, _, _ := strings.Cut(meta, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if !accepts[mediaType] {
		return imageRef{}, imageError(fmt.Sprintf(
			"inline image media type %q is not supported; %s are accepted",
			imageTypeInMessage(mediaType), imageTypesInMessage(accepts)))
	}
	if payload == "" {
		// An empty payload decodes fine and then vanishes: every adapter
		// keys the part on Base64 being non-empty, so the client was told
		// 200 for a message the model never saw an image in.
		return imageRef{}, imageError("inline image carries no payload")
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return imageRef{}, imageError("inline image payload is not valid standard base64, padding included")
	}
	return imageRef{MediaType: mediaType, Base64: payload}, nil
}

// imageDataIsBase64 reports the ";base64" RFC 2397 puts last among a data:
// URL's parameters. Anything else is a percent-encoded payload, which is not
// what any adapter forwards.
func imageDataIsBase64(meta string) bool {
	_, params, ok := strings.Cut(meta, ";")
	if !ok {
		return false
	}
	for _, p := range strings.Split(params, ";") {
		if strings.EqualFold(strings.TrimSpace(p), "base64") {
			return true
		}
	}
	return false
}

// imageTypeInMessage bounds a client-supplied media type before it goes into
// an error message: nothing about a data: URL limits how long the part before
// the comma is.
func imageTypeInMessage(s string) string {
	const max = 64
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
