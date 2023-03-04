package server

import (
	"fmt"
	"heckel.io/ntfy/log"
	"heckel.io/ntfy/user"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"heckel.io/ntfy/util"
)

const (
	// oneDay is an approximation of a day as a time.Duration
	oneDay = 24 * time.Hour

	// visitorExpungeAfter defines how long a visitor is active before it is removed from memory. This number
	// has to be very high to prevent e-mail abuse, but it doesn't really affect the other limits anyway, since
	// they are replenished faster (typically).
	visitorExpungeAfter = oneDay

	// visitorDefaultReservationsLimit is the amount of topic names a user without a tier is allowed to reserve.
	// This number is zero, and changing it may have unintended consequences in the web app, or otherwise
	visitorDefaultReservationsLimit = int64(0)
)

// Constants used to convert a tier-user's MessageLimit (see user.Tier) into adequate request limiter
// values (token bucket). This is only used to increase the values in server.yml, never decrease them.
//
// Example: Assuming a user.Tier's MessageLimit is 10,000:
// - the allowed burst is 500 (= 10,000 * 5%), which is < 1000 (the max)
// - the replenish rate is 2 * 10,000 / 24 hours
const (
	visitorMessageToRequestLimitBurstRate       = 0.05
	visitorMessageToRequestLimitBurstMax        = 1000
	visitorMessageToRequestLimitReplenishFactor = 2
)

// Constants used to convert a tier-user's EmailLimit (see user.Tier) into adequate email limiter
// values (token bucket). Example: Assuming a user.Tier's EmailLimit is 200, the allowed burst is
// 40 (= 200 * 20%), which is <150 (the max).
const (
	visitorEmailLimitBurstRate = 0.2
	visitorEmailLimitBurstMax  = 150
)

// visitor represents an API user, and its associated rate.Limiter used for rate limiting
type visitor struct {
	config              *Config
	messageCache        *messageCache
	userManager         *user.Manager      // May be nil
	ip                  netip.Addr         // Visitor IP address
	user                *user.User         // Only set if authenticated user, otherwise nil
	requestLimiter      *rate.Limiter      // Rate limiter for (almost) all requests (including messages)
	messagesLimiter     *util.FixedLimiter // Rate limiter for messages
	emailsLimiter       *util.RateLimiter  // Rate limiter for emails
	subscriptionLimiter *util.FixedLimiter // Fixed limiter for active subscriptions (ongoing connections)
	bandwidthLimiter    *util.RateLimiter  // Limiter for attachment bandwidth downloads
	accountLimiter      *rate.Limiter      // Rate limiter for account creation, may be nil
	authLimiter         *rate.Limiter      // Limiter for incorrect login attempts, may be nil
	firebase            time.Time          // Next allowed Firebase message
	seen                time.Time          // Last seen time of this visitor (needed for removal of stale visitors)
	mu                  sync.RWMutex
}

type visitorInfo struct {
	Limits *visitorLimits
	Stats  *visitorStats
}

type visitorLimits struct {
	Basis                    visitorLimitBasis
	RequestLimitBurst        int
	RequestLimitReplenish    rate.Limit
	MessageLimit             int64
	MessageExpiryDuration    time.Duration
	EmailLimit               int64
	EmailLimitBurst          int
	EmailLimitReplenish      rate.Limit
	ReservationsLimit        int64
	AttachmentTotalSizeLimit int64
	AttachmentFileSizeLimit  int64
	AttachmentExpiryDuration time.Duration
	AttachmentBandwidthLimit int64
}

type visitorStats struct {
	Messages                     int64
	MessagesRemaining            int64
	Emails                       int64
	EmailsRemaining              int64
	Reservations                 int64
	ReservationsRemaining        int64
	AttachmentTotalSize          int64
	AttachmentTotalSizeRemaining int64
}

// visitorLimitBasis describes how the visitor limits were derived, either from a user's
// IP address (default config), or from its tier
type visitorLimitBasis string

const (
	visitorLimitBasisIP   = visitorLimitBasis("ip")
	visitorLimitBasisTier = visitorLimitBasis("tier")
)

func newVisitor(conf *Config, messageCache *messageCache, userManager *user.Manager, ip netip.Addr, user *user.User) *visitor {
	var messages, emails int64
	if user != nil {
		messages = user.Stats.Messages
		emails = user.Stats.Emails
	}
	v := &visitor{
		config:              conf,
		messageCache:        messageCache,
		userManager:         userManager, // May be nil
		ip:                  ip,
		user:                user,
		firebase:            time.Unix(0, 0),
		seen:                time.Now(),
		subscriptionLimiter: util.NewFixedLimiter(int64(conf.VisitorSubscriptionLimit)),
		requestLimiter:      nil, // Set in resetLimiters
		messagesLimiter:     nil, // Set in resetLimiters, may be nil
		emailsLimiter:       nil, // Set in resetLimiters
		bandwidthLimiter:    nil, // Set in resetLimiters
		accountLimiter:      nil, // Set in resetLimiters, may be nil
		authLimiter:         nil, // Set in resetLimiters, may be nil
	}
	v.resetLimitersNoLock(messages, emails, false)
	return v
}

func (v *visitor) Context() log.Context {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.contextNoLock()
}

func (v *visitor) contextNoLock() log.Context {
	info := v.infoLightNoLock()
	fields := log.Context{
		"visitor_id":                     visitorID(v.ip, v.user),
		"visitor_ip":                     v.ip.String(),
		"visitor_seen":                   util.FormatTime(v.seen),
		"visitor_messages":               info.Stats.Messages,
		"visitor_messages_limit":         info.Limits.MessageLimit,
		"visitor_messages_remaining":     info.Stats.MessagesRemaining,
		"visitor_emails":                 info.Stats.Emails,
		"visitor_emails_limit":           info.Limits.EmailLimit,
		"visitor_emails_remaining":       info.Stats.EmailsRemaining,
		"visitor_request_limiter_limit":  v.requestLimiter.Limit(),
		"visitor_request_limiter_tokens": v.requestLimiter.Tokens(),
	}
	if v.authLimiter != nil {
		fields["visitor_auth_limiter_limit"] = v.authLimiter.Limit()
		fields["visitor_auth_limiter_tokens"] = v.authLimiter.Tokens()
	}
	if v.user != nil {
		fields["user_id"] = v.user.ID
		fields["user_name"] = v.user.Name
		if v.user.Tier != nil {
			for field, value := range v.user.Tier.Context() {
				fields[field] = value
			}
		}
		if v.user.Billing.StripeCustomerID != "" {
			fields["stripe_customer_id"] = v.user.Billing.StripeCustomerID
		}
		if v.user.Billing.StripeSubscriptionID != "" {
			fields["stripe_subscription_id"] = v.user.Billing.StripeSubscriptionID
		}
	}
	return fields
}

func visitorExtendedInfoContext(info *visitorInfo) log.Context {
	return log.Context{
		"visitor_reservations":                    info.Stats.Reservations,
		"visitor_reservations_limit":              info.Limits.ReservationsLimit,
		"visitor_reservations_remaining":          info.Stats.ReservationsRemaining,
		"visitor_attachment_total_size":           info.Stats.AttachmentTotalSize,
		"visitor_attachment_total_size_limit":     info.Limits.AttachmentTotalSizeLimit,
		"visitor_attachment_total_size_remaining": info.Stats.AttachmentTotalSizeRemaining,
	}

}
func (v *visitor) RequestAllowed() bool {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	return v.requestLimiter.Allow()
}

func (v *visitor) FirebaseAllowed() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return !time.Now().Before(v.firebase)
}

func (v *visitor) FirebaseTemporarilyDeny() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.firebase = time.Now().Add(v.config.FirebaseQuotaExceededPenaltyDuration)
}

func (v *visitor) MessageAllowed() bool {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	return v.messagesLimiter.Allow()
}

func (v *visitor) EmailAllowed() bool {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	return v.emailsLimiter.Allow()
}

func (v *visitor) SubscriptionAllowed() bool {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	return v.subscriptionLimiter.Allow()
}

// AuthAllowed returns true if an auth request can be attempted (> 1 token available)
func (v *visitor) AuthAllowed() bool {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	if v.authLimiter == nil {
		return true
	}
	return v.authLimiter.Tokens() > 1
}

// AuthFailed records an auth failure
func (v *visitor) AuthFailed() {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	if v.authLimiter != nil {
		v.authLimiter.Allow()
	}
}

// AccountCreationAllowed returns true if a new account can be created
func (v *visitor) AccountCreationAllowed() bool {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	if v.accountLimiter == nil || (v.accountLimiter != nil && v.accountLimiter.Tokens() < 1) {
		return false
	}
	return true
}

// AccountCreated decreases the account limiter. This is to be called after an account was created.
func (v *visitor) AccountCreated() {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	if v.accountLimiter != nil {
		v.accountLimiter.Allow()
	}
}

func (v *visitor) BandwidthAllowed(bytes int64) bool {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	return v.bandwidthLimiter.AllowN(bytes)
}

func (v *visitor) RemoveSubscription() {
	v.mu.RLock()
	defer v.mu.RUnlock()
	v.subscriptionLimiter.AllowN(-1)
}

func (v *visitor) Keepalive() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = time.Now()
}

func (v *visitor) BandwidthLimiter() util.Limiter {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	return v.bandwidthLimiter
}

func (v *visitor) Stale() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return time.Since(v.seen) > visitorExpungeAfter
}

func (v *visitor) Stats() *user.Stats {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	return &user.Stats{
		Messages: v.messagesLimiter.Value(),
		Emails:   v.emailsLimiter.Value(),
	}
}

func (v *visitor) ResetStats() {
	v.mu.RLock() // limiters could be replaced!
	defer v.mu.RUnlock()
	v.emailsLimiter.Reset()
	v.messagesLimiter.Reset()
}

// User returns the visitor user, or nil if there is none
func (v *visitor) User() *user.User {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.user // May be nil
}

// IP returns the visitor IP address
func (v *visitor) IP() netip.Addr {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.ip
}

// Authenticated returns true if a user successfully authenticated
func (v *visitor) Authenticated() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.user != nil
}

// SetUser sets the visitors user to the given value
func (v *visitor) SetUser(u *user.User) {
	v.mu.Lock()
	defer v.mu.Unlock()
	shouldResetLimiters := v.user.TierID() != u.TierID() // TierID works with nil receiver
	v.user = u                                           // u may be nil!
	if shouldResetLimiters {
		var messages, emails int64
		if u != nil {
			messages, emails = u.Stats.Messages, u.Stats.Emails
		}
		v.resetLimitersNoLock(messages, emails, true)
	}
}

// MaybeUserID returns the user ID of the visitor (if any). If this is an anonymous visitor,
// an empty string is returned.
func (v *visitor) MaybeUserID() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.user != nil {
		return v.user.ID
	}
	return ""
}

func (v *visitor) resetLimitersNoLock(messages, emails int64, enqueueUpdate bool) {
	limits := v.limitsNoLock()
	v.requestLimiter = rate.NewLimiter(limits.RequestLimitReplenish, limits.RequestLimitBurst)
	v.messagesLimiter = util.NewFixedLimiterWithValue(limits.MessageLimit, messages)
	v.emailsLimiter = util.NewRateLimiterWithValue(limits.EmailLimitReplenish, limits.EmailLimitBurst, emails)
	v.bandwidthLimiter = util.NewBytesLimiter(int(limits.AttachmentBandwidthLimit), oneDay)
	if v.user == nil {
		v.accountLimiter = rate.NewLimiter(rate.Every(v.config.VisitorAccountCreationLimitReplenish), v.config.VisitorAccountCreationLimitBurst)
		v.authLimiter = rate.NewLimiter(rate.Every(v.config.VisitorAuthFailureLimitReplenish), v.config.VisitorAuthFailureLimitBurst)
	} else {
		v.accountLimiter = nil // Users cannot create accounts when logged in
		v.authLimiter = nil    // Users are already logged in, no need to limit requests
	}
	if enqueueUpdate && v.user != nil {
		go v.userManager.EnqueueUserStats(v.user.ID, &user.Stats{
			Messages: messages,
			Emails:   emails,
		})
	}
	log.Fields(v.contextNoLock()).Debug("Rate limiters reset for visitor") // Must be after function, because contextNoLock() describes rate limiters
}

func (v *visitor) Limits() *visitorLimits {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.limitsNoLock()
}

func (v *visitor) limitsNoLock() *visitorLimits {
	if v.user != nil && v.user.Tier != nil {
		return tierBasedVisitorLimits(v.config, v.user.Tier)
	}
	return configBasedVisitorLimits(v.config)
}

func tierBasedVisitorLimits(conf *Config, tier *user.Tier) *visitorLimits {
	return &visitorLimits{
		Basis:                    visitorLimitBasisTier,
		RequestLimitBurst:        util.MinMax(int(float64(tier.MessageLimit)*visitorMessageToRequestLimitBurstRate), conf.VisitorRequestLimitBurst, visitorMessageToRequestLimitBurstMax),
		RequestLimitReplenish:    util.Max(rate.Every(conf.VisitorRequestLimitReplenish), dailyLimitToRate(tier.MessageLimit*visitorMessageToRequestLimitReplenishFactor)),
		MessageLimit:             tier.MessageLimit,
		MessageExpiryDuration:    tier.MessageExpiryDuration,
		EmailLimit:               tier.EmailLimit,
		EmailLimitBurst:          util.MinMax(int(float64(tier.EmailLimit)*visitorEmailLimitBurstRate), conf.VisitorEmailLimitBurst, visitorEmailLimitBurstMax),
		EmailLimitReplenish:      dailyLimitToRate(tier.EmailLimit),
		ReservationsLimit:        tier.ReservationLimit,
		AttachmentTotalSizeLimit: tier.AttachmentTotalSizeLimit,
		AttachmentFileSizeLimit:  tier.AttachmentFileSizeLimit,
		AttachmentExpiryDuration: tier.AttachmentExpiryDuration,
		AttachmentBandwidthLimit: tier.AttachmentBandwidthLimit,
	}
}

func configBasedVisitorLimits(conf *Config) *visitorLimits {
	messagesLimit := replenishDurationToDailyLimit(conf.VisitorRequestLimitReplenish) // Approximation!
	if conf.VisitorMessageDailyLimit > 0 {
		messagesLimit = int64(conf.VisitorMessageDailyLimit)
	}
	return &visitorLimits{
		Basis:                    visitorLimitBasisIP,
		RequestLimitBurst:        conf.VisitorRequestLimitBurst,
		RequestLimitReplenish:    rate.Every(conf.VisitorRequestLimitReplenish),
		MessageLimit:             messagesLimit,
		MessageExpiryDuration:    conf.CacheDuration,
		EmailLimit:               replenishDurationToDailyLimit(conf.VisitorEmailLimitReplenish), // Approximation!
		EmailLimitBurst:          conf.VisitorEmailLimitBurst,
		EmailLimitReplenish:      rate.Every(conf.VisitorEmailLimitReplenish),
		ReservationsLimit:        visitorDefaultReservationsLimit,
		AttachmentTotalSizeLimit: conf.VisitorAttachmentTotalSizeLimit,
		AttachmentFileSizeLimit:  conf.AttachmentFileSizeLimit,
		AttachmentExpiryDuration: conf.AttachmentExpiryDuration,
		AttachmentBandwidthLimit: conf.VisitorAttachmentDailyBandwidthLimit,
	}
}

func (v *visitor) Info() (*visitorInfo, error) {
	v.mu.RLock()
	info := v.infoLightNoLock()
	v.mu.RUnlock()

	// Attachment stats from database
	var attachmentsBytesUsed int64
	var err error
	u := v.User()
	if u != nil {
		attachmentsBytesUsed, err = v.messageCache.AttachmentBytesUsedByUser(u.ID)
	} else {
		attachmentsBytesUsed, err = v.messageCache.AttachmentBytesUsedBySender(v.IP().String())
	}
	if err != nil {
		return nil, err
	}
	info.Stats.AttachmentTotalSize = attachmentsBytesUsed
	info.Stats.AttachmentTotalSizeRemaining = zeroIfNegative(info.Limits.AttachmentTotalSizeLimit - attachmentsBytesUsed)

	// Reservation stats from database
	var reservations int64
	if v.userManager != nil && u != nil {
		reservations, err = v.userManager.ReservationsCount(u.Name)
		if err != nil {
			return nil, err
		}
	}
	info.Stats.Reservations = reservations
	info.Stats.ReservationsRemaining = zeroIfNegative(info.Limits.ReservationsLimit - reservations)

	return info, nil
}

func (v *visitor) infoLightNoLock() *visitorInfo {
	messages := v.messagesLimiter.Value()
	emails := v.emailsLimiter.Value()
	limits := v.limitsNoLock()
	stats := &visitorStats{
		Messages:          messages,
		MessagesRemaining: zeroIfNegative(limits.MessageLimit - messages),
		Emails:            emails,
		EmailsRemaining:   zeroIfNegative(limits.EmailLimit - emails),
	}
	return &visitorInfo{
		Limits: limits,
		Stats:  stats,
	}
}
func zeroIfNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func replenishDurationToDailyLimit(duration time.Duration) int64 {
	return int64(oneDay / duration)
}

func dailyLimitToRate(limit int64) rate.Limit {
	return rate.Limit(limit) * rate.Every(oneDay)
}

func visitorID(ip netip.Addr, u *user.User) string {
	if u != nil && u.Tier != nil {
		return fmt.Sprintf("user:%s", u.ID)
	}
	return fmt.Sprintf("ip:%s", ip.String())
}
