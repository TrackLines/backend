package billing

import "github.com/jackc/pgx/v5/pgxpool"

// Service holds what the checkout, portal and webhook handlers share.
// Without a Stripe key (or price/webhook secret/return URL) every billing endpoint answers 503.
type Service struct {
	DB            *pgxpool.Pool
	Stripe        Stripe
	PriceID       string
	WebhookSecret string
	ReturnURL     string // frontend settings page; checkout/portal send the user back here
}

func (s Service) configured() bool {
	return s.Stripe.SecretKey != "" && s.PriceID != "" && s.WebhookSecret != "" && s.ReturnURL != ""
}
