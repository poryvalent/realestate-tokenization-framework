// Command devseed creates a demo scheme, its people and an open offer with bids, for a LOCAL API.
//
// Directly in Postgres: a deployed LOCAL scheme (500 units, 25 held by the manager), its business clock, the
// investment manager with a wallet, and investors who can bid (verified KYC, demat and bank accounts, an anchor
// and a wallet each).
//
// Through the running API, so every row is exactly what the API itself would write: the offer is created and
// opened, and bids are placed. Bids must go through the API, because the ASBA sandbox lives in the API's memory;
// a bid inserted by SQL would carry a funds block the sandbox has never heard of, and settlement would fail.
//
// By default 250 investors are created and 240 bid two units each: 480 units against 475 on offer, over the
// 200-holder floor, so the ballot is a real draw. The other ten are left for placing a bid by hand in the UI.
//
//	go run ./cmd/devseed                         # API at ACRESYNC_HTTP_ADDR
//	go run ./cmd/devseed -api http://127.0.0.1:8080 -investors 250 -bids 240
//
// LOCAL only. The scheme is tagged LOCAL, which is also what lets cmd/devconfirm confirm its chain calls.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/config"
	"github.com/acresync/orchestrator/internal/db"
	"github.com/acresync/orchestrator/internal/devsim"
	"github.com/acresync/orchestrator/internal/entitlement"
	"github.com/acresync/orchestrator/internal/httpapi"
)

const unitPrice = 100000000 // ₹10 lakh in paise, the statutory minimum

type investor struct {
	ID     string `json:"investorId"`
	Demat  string `json:"dematAccountId"`
	Bank   string `json:"bankAccountId"`
	Wallet string `json:"wallet"`
	BidRef string `json:"bidRef,omitempty"`
}

type manifest struct {
	Warning       string      `json:"warning"`
	Stage         string      `json:"stage"`
	Period        *paidPeriod `json:"distributionPeriod,omitempty"`
	SchemeID      string      `json:"schemeId"`
	OfferID       string      `json:"offerId,omitempty"`
	BusinessTime  time.Time   `json:"businessTime"`
	ManagerID     string      `json:"investmentManagerInvestorId"`
	ManagerWallet string      `json:"investmentManagerWallet"`
	Bidders       []investor  `json:"bidders"`
	FreeInvestors []investor  `json:"investorsWithoutBids"`
	TokenHowTo    string      `json:"tokens"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devseed:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.FeatureDatabase)
	if err != nil {
		return err
	}
	api := flag.String("api", "http://"+cfg.HTTPAddr, "the running API's origin")
	count := flag.Int("investors", 250, "investors to create")
	bids := flag.Int("bids", 240, "of those, how many bid (0 creates no offer)")
	units := flag.Int("units", 2, "units per bid (1 to 25)")
	out := flag.String("out", "var/devseed.json", "where to write the manifest")
	stage := flag.String("stage", "open", "how far to take it: open (offer open with bids), settled (ballot drawn "+
		"and the register credited), paid (plus one closed distribution period with settled MOCK payouts)")
	flag.Parse()

	if *stage != "open" && *stage != "settled" && *stage != "paid" {
		return errors.New("-stage must be open, settled or paid")
	}
	if *stage != "open" && *bids < 200 {
		return errors.New("-stage settled and paid need at least 200 bids, or the offer cannot meet its holder floor")
	}

	if cfg.Environment != config.EnvLocal {
		return fmt.Errorf("refusing to seed %s; this tool is for LOCAL only", cfg.Environment)
	}
	if cfg.API.SessionSecret.IsZero() {
		return errors.New("ACRESYNC_API_SESSION_SECRET is not set; it must match the running API's")
	}
	if *bids > *count || *bids < 0 || *units < 1 || *units > 25 {
		return errors.New("need 0 <= bids <= investors and 1 <= units <= 25")
	}
	secret := []byte(cfg.API.SessionSecret.Reveal())
	*api = strings.TrimRight(*api, "/")

	ctx := context.Background()
	if *bids > 0 {
		// Fail before writing anything if the API is not there to take the bids.
		if err := ping(ctx, *api); err != nil {
			return fmt.Errorf("the API at %s is not answering (%w); start it first, with the same .env", *api, err)
		}
	}

	pool, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()

	now, err := clock.Real().Now(ctx)
	if err != nil {
		return err
	}
	business := now.Truncate(time.Hour)
	stamp := now.Format("20060102T150405")

	m := manifest{
		Stage:        "open",
		Warning:      "Demo data. LOCAL only. Chain calls are confirmed by cmd/devconfirm, which sends nothing anywhere.",
		TokenHowTo:   "go run ./cmd/devtoken -role MANAGER | -role TRUSTEE | -role COMPLIANCE | -investor <investorId>",
		BusinessTime: business,
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	ref := "SEBI/SM-REIT/DEMO/" + stamp
	addr := func(role string) string {
		sum := sha256.Sum256([]byte("acresync/devseed/" + role + "/" + ref))
		return "0x" + hex.EncodeToString(sum[:20])
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO schemes (
			sebi_scheme_ref, name, asset_value_paise, unit_price_paise,
			total_units, im_units, public_units, min_public_holders,
			status, chain_id, roles_address, ballot_address, scheme_address, environment_tag
		) VALUES ($1, $2, 50000000000, $3, 500, 25, 475, 200, 'DRAFT', 11155111, $4, $5, $6, 'LOCAL')
		RETURNING id`,
		ref, "AcreSync Demo Scheme "+stamp, unitPrice, addr("roles"), addr("ballot"), addr("scheme")).Scan(&m.SchemeID); err != nil {
		return fmt.Errorf("creating the scheme: %w", err)
	}

	leaseID, err := newAsset(ctx, tx, m.SchemeID, stamp)
	if err != nil {
		return err
	}

	mgr, err := newInvestor(ctx, tx, m.SchemeID, "manager", false)
	if err != nil {
		return err
	}
	m.ManagerID, m.ManagerWallet = mgr.ID, mgr.Wallet
	if _, err := tx.Exec(ctx, `UPDATE schemes SET im_investor_id = $2 WHERE id = $1`, m.SchemeID, mgr.ID); err != nil {
		return err
	}

	people := make([]investor, 0, *count)
	details := map[string]entitlement.HolderDetail{
		mgr.ID: {InvestorID: mgr.ID, Class: entitlement.ClassBodyCorporate, BankAccountID: mgr.Bank, FundAccountID: "fa_00000000000000"},
	}
	for i := 0; i < *count; i++ {
		p, err := newInvestor(ctx, tx, m.SchemeID, fmt.Sprintf("%03d", i), true)
		if err != nil {
			return err
		}
		people = append(people, p)
		details[p.ID] = entitlement.HolderDetail{InvestorID: p.ID, Class: entitlement.ClassResidentIndividual,
			BankAccountID: p.Bank, FundAccountID: fmt.Sprintf("fa_%014d", i+1)}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	if _, err := clock.NewPostgresStore(pool.Pool).Init(ctx, m.SchemeID, business); err != nil {
		return fmt.Errorf("starting the scheme's business clock: %w", err)
	}
	fmt.Printf("seeded scheme %s: manager + %d investors, business time %s\n", m.SchemeID, *count, business.Format(time.RFC3339))

	if *bids == 0 {
		m.FreeInvestors = people
		return write(*out, m)
	}

	c := &client{base: *api, secret: secret, run: strings.ToLower(stamp)}
	mgrTok, err := c.token(httpapi.Claims{Kind: httpapi.PrincipalOperator, Role: httpapi.RoleManager, Subject: "devseed|manager"})
	if err != nil {
		return err
	}
	var offer struct {
		ID string `json:"id"`
	}
	if err := c.post("/v1/admin/offers", mgrTok, "offer", map[string]any{
		"schemeId": m.SchemeID, "offerType": "INITIAL",
		"terms": map[string]any{
			"unitsOnOffer": 475, "minBidUnits": 1, "maxBidUnits": 25,
			"minSubscriptionUnits": 428, "minDistinctHolders": 200,
			"priceBandLowerPaise": unitPrice, "priceBandUpperPaise": 105000000,
			"opensAt":        business.Add(-time.Hour).Format(time.RFC3339),
			"closesAt":       business.Add(72 * time.Hour).Format(time.RFC3339),
			"allotmentDueAt": business.Add(96 * time.Hour).Format(time.RFC3339),
		},
	}, http.StatusCreated, &offer); err != nil {
		return fmt.Errorf("creating the offer: %w", err)
	}
	m.OfferID = offer.ID
	if err := c.post("/v1/admin/offers/"+offer.ID+"/transitions", mgrTok, "open", map[string]any{"to": "OPEN"}, http.StatusOK, nil); err != nil {
		return fmt.Errorf("opening the offer: %w", err)
	}
	fmt.Printf("offer %s is OPEN; placing %d bids of %d units\n", offer.ID, *bids, *units)

	for i := 0; i < *bids; i++ {
		p := &people[i]
		tok, err := c.token(httpapi.Claims{Kind: httpapi.PrincipalInvestor, InvestorID: p.ID, Subject: "devseed|" + p.ID})
		if err != nil {
			return err
		}
		var bid struct {
			BidRef string `json:"bidRef"`
			Status string `json:"status"`
		}
		if err := c.post("/v1/offers/"+offer.ID+"/bids", tok, fmt.Sprintf("bid-%03d", i), map[string]any{
			"unitsBid": *units, "pricePerUnitPaise": unitPrice, "dematAccountId": p.Demat, "bankAccountId": p.Bank,
		}, http.StatusCreated, &bid); err != nil {
			return fmt.Errorf("bid %d: %w", i, err)
		}
		if bid.Status != "FUNDS_BLOCKED" {
			return fmt.Errorf("bid %d came back %s, want FUNDS_BLOCKED", i, bid.Status)
		}
		p.BidRef = bid.BidRef
	}
	m.Bidders, m.FreeInvestors = people[:*bids], people[*bids:]
	fmt.Printf("placed %d bids (%d units against 475 on offer)\n", *bids, *bids**units)
	if *stage == "open" {
		return write(*out, m)
	}

	head, closeHead, err := devsim.HeadFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeHead()
	if err := settle(ctx, c, mgrTok, pool, head, m.SchemeID, offer.ID); err != nil {
		return fmt.Errorf("settling the offer: %w", err)
	}
	m.Stage = "settled"
	fmt.Println("offer SETTLED: ballot drawn, 475 units credited, manager's 25 recorded (chain confirmations simulated)")

	if *stage == "paid" {
		paid, err := seedPaidPeriod(ctx, cfg, pool, head, &m, details, leaseID)
		if err != nil {
			return fmt.Errorf("seeding the distribution period: %w", err)
		}
		m.Stage, m.Period = "paid", paid
		fmt.Printf("period %d CLOSED: INR %d distributed, %d MOCK payouts settled\n",
			paid.PeriodSeq, paid.Distributed/100, paid.Payouts)
	}
	return write(*out, m)
}

// settle drives the offer from OPEN to SETTLED through the API, confirming each chain call as devconfirm would.
//
// Every step is the endpoint the console calls, so the resulting rows are exactly what a clicked-through demo
// produces. The confirmations are simulated, which is the one thing a console user could not do by clicking.
func settle(ctx context.Context, c *client, tok string, pool *db.Pool, head devsim.Head, schemeID, offerID string) error {
	confirm := func() error { return confirmChain(ctx, pool, head, schemeID) }
	base := "/v1/admin/offers/" + offerID
	steps := []struct {
		name, path string
		body       any
		want       int
	}{
		{"close", base + "/transitions", map[string]any{"to": "CLOSED"}, http.StatusOK},
		{"freeze", base + "/book/freeze", nil, http.StatusOK},
		{"commit", base + "/ballot/commit", nil, http.StatusAccepted},
		{"reveal", base + "/ballot/reveal", nil, http.StatusAccepted},
		{"draw", base + "/ballot/draw", nil, http.StatusOK},
		{"finalise-allotment", base + "/transitions", map[string]any{"to": "ALLOTMENT_FINALISED"}, http.StatusOK},
		{"begin", base + "/settlement/begin", nil, http.StatusAccepted},
	}
	for _, s := range steps {
		if err := c.post(s.path, tok, s.name, s.body, s.want, nil); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		if err := confirm(); err != nil {
			return err
		}
	}
	for i := 0; ; i++ {
		var st struct {
			Stage           string  `json:"stage"`
			CreditedHolders int     `json:"creditedHolders"`
			NextStep        *string `json:"nextStep"`
		}
		if err := c.get(base+"/settlement", tok, &st); err != nil {
			return err
		}
		switch {
		case st.Stage == "FINALISED":
			return nil
		case i > 20:
			return fmt.Errorf("settlement stopped at stage %s, cursor %d", st.Stage, st.CreditedHolders)
		case st.NextStep != nil && *st.NextStep == "settleBatch":
			if err := c.post(base+"/settlement/batches", tok, fmt.Sprintf("batch-%d", st.CreditedHolders),
				map[string]any{"cursorFrom": st.CreditedHolders}, http.StatusAccepted, nil); err != nil {
				return fmt.Errorf("batch at %d: %w", st.CreditedHolders, err)
			}
		case st.NextStep != nil && *st.NextStep == "finaliseSettlement":
			if err := c.post(base+"/settlement/finalise", tok, "finalise", nil, http.StatusAccepted, nil); err != nil {
				return fmt.Errorf("finalise: %w", err)
			}
		}
		if err := confirm(); err != nil {
			return err
		}
	}
}

// newAsset gives the scheme an SPV, a property and a lease, which rent receipts are recorded against.
func newAsset(ctx context.Context, tx pgx.Tx, schemeID, stamp string) (string, error) {
	sum := sha256.Sum256([]byte("acresync/devseed/cin/" + schemeID))
	cin := fmt.Sprintf("U70100KA26PTC%06d", (uint32(sum[0])<<16|uint32(sum[1])<<8|uint32(sum[2]))%1000000)
	var spv, prop, lease string
	if err := tx.QueryRow(ctx, `
		INSERT INTO spvs (scheme_id, cin, name, incorporation_date) VALUES ($1, $2, $3, '2025-04-01') RETURNING id`,
		schemeID, cin, "AcreSync Demo SPV "+stamp).Scan(&spv); err != nil {
		return "", fmt.Errorf("creating the SPV: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO properties (spv_id, name, address_text, city, grade, carpet_area_sqft, leasable_area_sqft, acquisition_value_paise)
		VALUES ($1, 'Demo Business Park', '1 Demo Road', 'Bengaluru', 'A', 80000, 100000, 50000000000) RETURNING id`,
		spv).Scan(&prop); err != nil {
		return "", fmt.Errorf("creating the property: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO leases (property_id, tenant_name, monthly_rent_paise, start_date, end_date)
		VALUES ($1, 'Demo Anchor Tenant Pvt Ltd', 380000000, '2025-04-01', '2035-03-31') RETURNING id`,
		prop).Scan(&lease); err != nil {
		return "", fmt.Errorf("creating the lease: %w", err)
	}
	return lease, nil
}

// newInvestor creates an investor with accounts, verified KYC, an anchor for the scheme, and a wallet.
//
// No identity is stored in the clear anywhere, same as the real schema expects: the name and PAN columns are
// placeholders for encrypted blobs. The anchor is a plain hash rather than an HMAC under the KMS pepper, which
// is fine for a demo and is exactly why this tool is LOCAL only.
func newInvestor(ctx context.Context, tx pgx.Tx, schemeID, label string, bidder bool) (investor, error) {
	var p investor
	blob := []byte("enc:devseed:" + label + ":" + schemeID)
	class := "RESIDENT_IND"
	if !bidder {
		class = "BODY_CORPORATE" // the investment manager is a company
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO investors (full_name_enc, pan_enc, email_enc, phone_enc, investor_class)
		VALUES ($1, $1, $1, $1, $2) RETURNING id`, blob, class).Scan(&p.ID); err != nil {
		return p, fmt.Errorf("creating investor %s: %w", label, err)
	}
	sum := sha256.Sum256([]byte("acresync/devseed/wallet/" + p.ID))
	p.Wallet = "0x" + hex.EncodeToString(sum[:20])
	if _, err := tx.Exec(ctx, `INSERT INTO wallets (investor_id, address) VALUES ($1, $2)`, p.ID, p.Wallet); err != nil {
		return p, err
	}
	// Every holder needs an anchor for the register snapshot and a bank account to be paid into, the manager
	// included: its 25 units earn distributions like any other.
	anchor := sha256.Sum256([]byte("acresync/devseed/anchor/" + schemeID + "/" + p.ID))
	if _, err := tx.Exec(ctx, `
		INSERT INTO investor_anchors (investor_id, scheme_id, anchor_hash, pepper_key_id)
		VALUES ($1, $2, $3, 'devseed')`, p.ID, schemeID, anchor[:]); err != nil {
		return p, err
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO bank_accounts (investor_id, account_number_enc, ifsc, account_name_enc, verified_at)
		VALUES ($1, $2, 'HDFC0000001', $2, now()) RETURNING id`, p.ID, blob).Scan(&p.Bank); err != nil {
		return p, err
	}
	if !bidder {
		return p, nil
	}
	short := strings.ReplaceAll(p.ID, "-", "")[:16]
	if err := tx.QueryRow(ctx, `
		INSERT INTO demat_accounts (investor_id, depository, dp_id, client_id)
		VALUES ($1, 'NSDL', $2, $3) RETURNING id`, p.ID, "IN"+short[:6], short).Scan(&p.Demat); err != nil {
		return p, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO kyc_records (investor_id, provider, kyc_ref, status, verified_at, expires_at)
		VALUES ($1, 'MOCK', $2, 'VERIFIED', now(), now() + interval '1 year')`, p.ID, "DEVSEED-"+short); err != nil {
		return p, err
	}
	return p, nil
}

type client struct {
	base   string
	secret []byte
	run    string
	http   http.Client
}

func (c *client) token(cl httpapi.Claims) (string, error) {
	now, err := clock.Real().Now(context.Background())
	if err != nil {
		return "", err
	}
	cl.IssuedAt, cl.ExpiresAt = now.Unix(), now.Add(10*time.Minute).Unix()
	return httpapi.MintDevToken(c.secret, cl)
}

// post sends one write. The idempotency key is fixed per seed run and step, so a rerun of a failed step replays.
func (c *client) post(path, token, step string, body any, want int, into any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "devseed-"+c.run+"-"+step)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != want {
		return fmt.Errorf("POST %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(got)))
	}
	if into != nil {
		return json.Unmarshal(got, into)
	}
	return nil
}

func (c *client) get(path, token string, into any) error {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(got)))
	}
	return json.Unmarshal(got, into)
}

func ping(ctx context.Context, base string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func write(path string, m manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	fmt.Printf("manifest written to %s (investor ids for devtoken are in it)\n", path)
	return nil
}
