package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type PortalMembership struct {
	WorkspaceID string
	TenantSlug  string
	TenantName  string
	Kind        string
	Role        string
	CreatedAt   *time.Time
}

type PortalInvitation struct {
	ID         string
	TenantSlug string
	Email      string
	Role       string
	Status     string
	InvitedBy  string
	CreatedAt  *time.Time
}

type PortalUser struct {
	ID            string
	Email         string
	Company       string
	Plan          string
	WorkspaceID   string
	TenantSlug    string
	TenantName    string
	Role          string
	HasPAT        bool
	PATLastUsedAt *time.Time
	CreatedAt     *time.Time
	Memberships   []PortalMembership
}

func (u PortalUser) HasTenant() bool {
	return strings.TrimSpace(u.TenantSlug) != ""
}

func generateWorkspaceSlug(prefix string) (string, error) {
	suffix, err := randomHex(6)
	if err != nil {
		return "", err
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "ws"
	}
	return prefix + suffix, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashSecret(salt, secret string) string {
	sum := sha256.Sum256([]byte(salt + ":" + secret))
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreatePortalUser(ctx context.Context, email, company, plan, tenantSlug, password string, autoDefaultEnv bool) (PortalUser, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	company = strings.TrimSpace(company)
	plan = strings.TrimSpace(plan)
	tenantSlug = strings.TrimSpace(tenantSlug)
	if email == "" || tenantSlug == "" || password == "" {
		return PortalUser{}, fmt.Errorf("email, tenant slug and password are required")
	}
	if plan == "" {
		plan = "starter"
	}
	if company == "" {
		company = tenantSlug
	}
	if _, err := s.GetTenantBySlug(ctx, tenantSlug); err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			if _, cerr := s.CreateTenantWithDefaultEnvironment(ctx, tenantSlug, company, autoDefaultEnv); cerr != nil {
				return PortalUser{}, cerr
			}
		} else {
			return PortalUser{}, err
		}
	}

	salt, err := randomToken(16)
	if err != nil {
		return PortalUser{}, err
	}
	hash := hashSecret(salt, password)

	var out PortalUser
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var existing int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenant_memberships WHERE tenant_slug = $1 AND revoked_at IS NULL`, tenantSlug).Scan(&existing); err != nil {
			return err
		}
		role := "member"
		if existing == 0 {
			role = "owner"
		}

		if err := tx.QueryRow(ctx, `
INSERT INTO portal_accounts (email, company, plan, password_salt, password_hash)
VALUES ($1,$2,$3,$4,$5)
RETURNING id::text, email, company, plan, created_at
`, email, company, plan, salt, hash).Scan(&out.ID, &out.Email, &out.Company, &out.Plan, &out.CreatedAt); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
INSERT INTO tenant_memberships (tenant_slug, account_id, role)
VALUES ($1,$2,$3)
`, tenantSlug, out.ID, role); err != nil {
			return err
		}

		out.TenantSlug = tenantSlug
		out.Role = role
		out.Memberships = []PortalMembership{{TenantSlug: tenantSlug, TenantName: company, Kind: "organization", Role: role}}
		return nil
	})
	return out, err
}

func (s *Store) CreatePortalAccount(ctx context.Context, email, company, plan, password string) (PortalUser, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	company = strings.TrimSpace(company)
	plan = strings.TrimSpace(plan)
	if email == "" || password == "" {
		return PortalUser{}, fmt.Errorf("email and password are required")
	}
	if plan == "" {
		plan = "starter"
	}
	salt, err := randomToken(16)
	if err != nil {
		return PortalUser{}, err
	}
	hash := hashSecret(salt, password)
	var out PortalUser
	err = s.pool.QueryRow(ctx, `
INSERT INTO portal_accounts (email, company, plan, password_salt, password_hash)
VALUES ($1,$2,$3,$4,$5)
RETURNING id::text, email, company, plan, created_at
`, email, company, plan, salt, hash).Scan(&out.ID, &out.Email, &out.Company, &out.Plan, &out.CreatedAt)
	return out, err
}

func (s *Store) FindOrCreatePortalOIDCAccount(ctx context.Context, email, company, plan, provider string) (PortalUser, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	company = strings.TrimSpace(company)
	plan = strings.TrimSpace(plan)
	provider = strings.TrimSpace(provider)
	if email == "" {
		return PortalUser{}, fmt.Errorf("email is required")
	}
	if plan == "" {
		plan = "starter"
	}
	var out PortalUser
	err := s.pool.QueryRow(ctx, `
INSERT INTO portal_accounts (email, company, plan, password_salt, password_hash, password_login_enabled, auth_source, oidc_provider)
VALUES ($1,$2,$3,'','',false,'oidc',$4)
ON CONFLICT (email) DO UPDATE
   SET company = CASE WHEN COALESCE(portal_accounts.company, '') = '' AND EXCLUDED.company <> '' THEN EXCLUDED.company ELSE portal_accounts.company END,
       plan = CASE WHEN COALESCE(portal_accounts.plan, '') = '' AND EXCLUDED.plan <> '' THEN EXCLUDED.plan ELSE portal_accounts.plan END,
       auth_source = CASE WHEN portal_accounts.auth_source = 'password' THEN portal_accounts.auth_source ELSE 'oidc' END,
       oidc_provider = CASE WHEN EXCLUDED.oidc_provider <> '' THEN EXCLUDED.oidc_provider ELSE portal_accounts.oidc_provider END,
       updated_at = now()
RETURNING id::text, email, company, plan, created_at
`, email, company, plan, provider).Scan(&out.ID, &out.Email, &out.Company, &out.Plan, &out.CreatedAt)
	if err != nil {
		return PortalUser{}, err
	}
	memberships, err := s.ListPortalMembershipsByAccount(ctx, out.ID)
	if err != nil {
		return PortalUser{}, err
	}
	out.Memberships = memberships
	applyActiveMembership(&out, "", memberships)
	return out, nil
}

func (s *Store) AuthenticatePortalUser(ctx context.Context, email, password string) (PortalUser, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var (
		out                  PortalUser
		salt                 string
		hash                 string
		passwordLoginEnabled bool
	)
	err := s.pool.QueryRow(ctx, `
SELECT id::text, email, company, plan, password_salt, password_hash, password_login_enabled, created_at
FROM portal_accounts WHERE email = $1
`, email).Scan(&out.ID, &out.Email, &out.Company, &out.Plan, &salt, &hash, &passwordLoginEnabled, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortalUser{}, fmt.Errorf("invalid credentials")
	}
	if err != nil {
		return PortalUser{}, err
	}
	if !passwordLoginEnabled {
		return PortalUser{}, fmt.Errorf("invalid credentials")
	}
	got := hashSecret(salt, password)
	if subtle.ConstantTimeCompare([]byte(got), []byte(hash)) != 1 {
		return PortalUser{}, fmt.Errorf("invalid credentials")
	}
	memberships, err := s.ListPortalMembershipsByAccount(ctx, out.ID)
	if err != nil {
		return PortalUser{}, err
	}
	out.Memberships = memberships
	applyActiveMembership(&out, "", memberships)
	return out, nil
}

func applyActiveMembership(out *PortalUser, activeTenant string, memberships []PortalMembership) {
	activeTenant = strings.TrimSpace(activeTenant)
	if len(memberships) == 0 {
		out.WorkspaceID = ""
		out.TenantSlug = ""
		out.TenantName = ""
		out.Role = ""
		return
	}
	if activeTenant != "" {
		for _, membership := range memberships {
			if membership.TenantSlug == activeTenant {
				out.WorkspaceID = membership.WorkspaceID
				out.TenantSlug = membership.TenantSlug
				out.TenantName = membership.TenantName
				out.Role = membership.Role
				return
			}
		}
	}
	out.WorkspaceID = memberships[0].WorkspaceID
	out.TenantSlug = memberships[0].TenantSlug
	out.TenantName = memberships[0].TenantName
	out.Role = memberships[0].Role
}

func (s *Store) CreatePortalSession(ctx context.Context, accountID string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	token, err := randomToken(24)
	if err != nil {
		return "", err
	}
	tokenHash := hashSecret("portal_session", token)
	memberships, err := s.ListPortalMembershipsByAccount(ctx, accountID)
	if err != nil {
		return "", err
	}
	activeTenant := ""
	if len(memberships) > 0 {
		activeTenant = memberships[0].TenantSlug
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO portal_sessions (account_id, active_tenant_slug, token_hash, expires_at)
VALUES ($1,$2,$3,$4)
`, accountID, portalNullIfEmpty(activeTenant), tokenHash, time.Now().UTC().Add(ttl))
	if err != nil {
		return "", err
	}
	return token, nil
}

func portalNullIfEmpty(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) DeletePortalSession(ctx context.Context, token string) error {
	tokenHash := hashSecret("portal_session", token)
	_, err := s.pool.Exec(ctx, `DELETE FROM portal_sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (s *Store) GetPortalSessionUser(ctx context.Context, token string) (PortalUser, error) {
	tokenHash := hashSecret("portal_session", token)
	var out PortalUser
	var activeTenant string
	err := s.pool.QueryRow(ctx, `
SELECT a.id::text, a.email, a.company, a.plan, a.created_at, COALESCE(s.active_tenant_slug, '')
FROM portal_sessions s
JOIN portal_accounts a ON a.id = s.account_id
WHERE s.token_hash = $1
  AND s.expires_at > now()
`, tokenHash).Scan(&out.ID, &out.Email, &out.Company, &out.Plan, &out.CreatedAt, &activeTenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortalUser{}, fmt.Errorf("unauthenticated")
	}
	if err != nil {
		return PortalUser{}, err
	}
	memberships, err := s.ListPortalMembershipsByAccount(ctx, out.ID)
	if err != nil {
		return PortalUser{}, err
	}
	out.Memberships = memberships
	applyActiveMembership(&out, activeTenant, memberships)
	return out, nil
}

func (s *Store) SetPortalSessionActiveTenant(ctx context.Context, token, tenantSlug string) error {
	tokenHash := hashSecret("portal_session", token)
	tenantSlug = strings.TrimSpace(tenantSlug)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var accountID string
		err := tx.QueryRow(ctx, `
SELECT account_id::text
FROM portal_sessions
WHERE token_hash = $1
  AND expires_at > now()
FOR UPDATE`, tokenHash).Scan(&accountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("unauthenticated")
		}
		if err != nil {
			return err
		}
		if tenantSlug != "" {
			var count int
			if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM tenant_memberships
WHERE account_id = $1
  AND tenant_slug = $2
  AND revoked_at IS NULL`, accountID, tenantSlug).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return fmt.Errorf("account is not a member of tenant %q", tenantSlug)
			}
		}
		_, err = tx.Exec(ctx, `
UPDATE portal_sessions
SET active_tenant_slug = $2
WHERE token_hash = $1`, tokenHash, portalNullIfEmpty(tenantSlug))
		return err
	})
}

func (s *Store) ListPortalMembershipsByAccount(ctx context.Context, accountID string) ([]PortalMembership, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, fmt.Errorf("account id is required")
	}
	rows, err := s.pool.Query(ctx, `
SELECT t.workspace_id, tm.tenant_slug, t.name, t.kind, tm.role, tm.created_at
FROM tenant_memberships tm
JOIN tenants t ON t.slug = tm.tenant_slug
WHERE account_id = $1
  AND revoked_at IS NULL
  AND t.lifecycle_status = 'active'
ORDER BY CASE role WHEN 'owner' THEN 0 WHEN 'tenant_admin' THEN 1 WHEN 'billing_admin' THEN 2 ELSE 3 END,
         created_at ASC
`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortalMembership
	for rows.Next() {
		var m PortalMembership
		if err := rows.Scan(&m.WorkspaceID, &m.TenantSlug, &m.TenantName, &m.Kind, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListPortalUsersByTenant(ctx context.Context, tenantSlug string) ([]PortalUser, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	if tenantSlug == "" {
		return nil, fmt.Errorf("tenant slug is required")
	}
	rows, err := s.pool.Query(ctx, `
SELECT a.id::text, a.email, a.company, a.plan, m.tenant_slug, t.name, t.kind, m.role,
       EXISTS (
         SELECT 1
         FROM portal_personal_access_tokens pat
         WHERE pat.account_id = a.id
           AND pat.revoked_at IS NULL
       ) AS has_pat,
       (
         SELECT pat.last_used_at
         FROM portal_personal_access_tokens pat
         WHERE pat.account_id = a.id
           AND pat.revoked_at IS NULL
         ORDER BY pat.created_at DESC
         LIMIT 1
       ) AS pat_last_used_at,
       a.created_at
FROM tenant_memberships m
JOIN portal_accounts a ON a.id = m.account_id
JOIN tenants t ON t.slug = m.tenant_slug
WHERE m.tenant_slug = $1
  AND m.revoked_at IS NULL
ORDER BY a.created_at ASC`, tenantSlug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortalUser
	for rows.Next() {
		var u PortalUser
		var tenantName string
		var kind string
		if err := rows.Scan(&u.ID, &u.Email, &u.Company, &u.Plan, &u.TenantSlug, &tenantName, &kind, &u.Role, &u.HasPAT, &u.PATLastUsedAt, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.TenantName = tenantName
		u.Memberships = []PortalMembership{{TenantSlug: u.TenantSlug, TenantName: tenantName, Kind: kind, Role: u.Role}}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CreateTenantInvitation(ctx context.Context, tenantSlug, email, role, invitedBy string) (PortalInvitation, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	email = strings.ToLower(strings.TrimSpace(email))
	role = strings.ToLower(strings.TrimSpace(role))
	invitedBy = strings.TrimSpace(invitedBy)
	if tenantSlug == "" || email == "" {
		return PortalInvitation{}, fmt.Errorf("tenant slug and email are required")
	}
	switch role {
	case "", "member":
		role = "member"
	case "tenant_admin", "billing_admin":
	default:
		return PortalInvitation{}, fmt.Errorf("invalid invite role %q", role)
	}
	var out PortalInvitation
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var tenantKind string
		if err := tx.QueryRow(ctx, `SELECT kind FROM tenants WHERE slug = $1`, tenantSlug).Scan(&tenantKind); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTenantNotFound
			}
			return err
		}
		if tenantKind == "personal" {
			return fmt.Errorf("personal workspaces cannot invite additional users")
		}
		var existing int
		if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM tenant_memberships tm
JOIN portal_accounts a ON a.id = tm.account_id
WHERE tm.tenant_slug = $1
  AND a.email = $2
  AND tm.revoked_at IS NULL`, tenantSlug, email).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			return fmt.Errorf("%s already belongs to tenant %q", email, tenantSlug)
		}
		if err := tx.QueryRow(ctx, `
INSERT INTO tenant_invitations (tenant_slug, email, role, status, invited_by)
VALUES ($1,$2,$3,'pending',$4)
RETURNING id::text, tenant_slug, email, role, status, invited_by, created_at
`, tenantSlug, email, role, invitedBy).Scan(&out.ID, &out.TenantSlug, &out.Email, &out.Role, &out.Status, &out.InvitedBy, &out.CreatedAt); err != nil {
			return err
		}
		return nil
	})
	return out, err
}

func (s *Store) ListTenantInvitations(ctx context.Context, tenantSlug string) ([]PortalInvitation, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	if tenantSlug == "" {
		return nil, fmt.Errorf("tenant slug is required")
	}
	rows, err := s.pool.Query(ctx, `
SELECT id::text, tenant_slug, email, role, status, invited_by, created_at
FROM tenant_invitations
WHERE tenant_slug = $1
ORDER BY created_at DESC`, tenantSlug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortalInvitation
	for rows.Next() {
		var inv PortalInvitation
		if err := rows.Scan(&inv.ID, &inv.TenantSlug, &inv.Email, &inv.Role, &inv.Status, &inv.InvitedBy, &inv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *Store) ListPortalInvitationsByEmail(ctx context.Context, email string) ([]PortalInvitation, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}
	rows, err := s.pool.Query(ctx, `
SELECT id::text, tenant_slug, email, role, status, invited_by, created_at
FROM tenant_invitations
WHERE email = $1
  AND status = 'pending'
ORDER BY created_at DESC`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortalInvitation
	for rows.Next() {
		var inv PortalInvitation
		if err := rows.Scan(&inv.ID, &inv.TenantSlug, &inv.Email, &inv.Role, &inv.Status, &inv.InvitedBy, &inv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *Store) AcceptTenantInvitation(ctx context.Context, invitationID, accountID, email string) (PortalMembership, error) {
	invitationID = strings.TrimSpace(invitationID)
	accountID = strings.TrimSpace(accountID)
	email = strings.ToLower(strings.TrimSpace(email))
	if invitationID == "" || accountID == "" || email == "" {
		return PortalMembership{}, fmt.Errorf("invitation id, account id, and email are required")
	}
	var membership PortalMembership
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var inv PortalInvitation
		err := tx.QueryRow(ctx, `
SELECT id::text, tenant_slug, email, role, status, invited_by, created_at
FROM tenant_invitations
WHERE id = $1
FOR UPDATE`, invitationID).Scan(&inv.ID, &inv.TenantSlug, &inv.Email, &inv.Role, &inv.Status, &inv.InvitedBy, &inv.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("invitation not found")
		}
		if err != nil {
			return err
		}
		if inv.Status != "pending" {
			return fmt.Errorf("invitation is %s", inv.Status)
		}
		if !strings.EqualFold(inv.Email, email) {
			return fmt.Errorf("invitation email does not match current account")
		}
		_, err = tx.Exec(ctx, `
INSERT INTO tenant_memberships (tenant_slug, account_id, role)
VALUES ($1,$2,$3)
ON CONFLICT (tenant_slug, account_id) WHERE revoked_at IS NULL
DO UPDATE SET role = EXCLUDED.role, updated_at = now()`, inv.TenantSlug, accountID, inv.Role)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
SELECT tm.tenant_slug, t.name, t.kind, tm.role, tm.created_at
FROM tenant_memberships tm
JOIN tenants t ON t.slug = tm.tenant_slug
WHERE tm.tenant_slug = $1
  AND tm.account_id = $2
  AND tm.revoked_at IS NULL`, inv.TenantSlug, accountID).Scan(&membership.TenantSlug, &membership.TenantName, &membership.Kind, &membership.Role, &membership.CreatedAt); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
UPDATE tenant_invitations
SET status = 'accepted',
    responded_at = now()
WHERE id = $1`, invitationID)
		return err
	})
	return membership, err
}

func (s *Store) RejectTenantInvitation(ctx context.Context, invitationID, email string) error {
	invitationID = strings.TrimSpace(invitationID)
	email = strings.ToLower(strings.TrimSpace(email))
	if invitationID == "" || email == "" {
		return fmt.Errorf("invitation id and email are required")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE tenant_invitations
SET status = 'rejected',
    responded_at = now()
WHERE id = $1
  AND email = $2
  AND status = 'pending'`, invitationID, email)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("invitation not found")
	}
	return nil
}

func (s *Store) CancelTenantInvitation(ctx context.Context, tenantSlug, invitationID, actor string) error {
	tenantSlug = strings.TrimSpace(tenantSlug)
	invitationID = strings.TrimSpace(invitationID)
	actor = strings.TrimSpace(actor)
	if tenantSlug == "" || invitationID == "" {
		return fmt.Errorf("tenant slug and invitation id are required")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var oldStatus string
		err := tx.QueryRow(ctx, `
SELECT status
FROM tenant_invitations
WHERE id = $1
  AND tenant_slug = $2
FOR UPDATE`, invitationID, tenantSlug).Scan(&oldStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("invitation not found")
		}
		if err != nil {
			return err
		}
		if oldStatus != "pending" {
			return fmt.Errorf("invitation is %s", oldStatus)
		}
		tag, err := tx.Exec(ctx, `
UPDATE tenant_invitations
SET status = 'cancelled',
    responded_at = now()
WHERE id = $1
  AND tenant_slug = $2
  AND status = 'pending'`, invitationID, tenantSlug)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("invitation not found")
		}
		var tenantID string
		_ = tx.QueryRow(ctx, `SELECT id::text FROM tenants WHERE slug = $1`, tenantSlug).Scan(&tenantID)
		_ = insertControlEvent(ctx, tx, "portal_invitation_cancel", tenantID, actor, map[string]any{
			"tenant_slug":    tenantSlug,
			"invitation_id":  invitationID,
			"previous_state": oldStatus,
		})
		return nil
	})
}

func (s *Store) CreateTenantForPortalAccount(ctx context.Context, accountID, tenantSlug, tenantName, actor string, autoDefaultEnv bool) (TenantRow, PortalMembership, error) {
	accountID = strings.TrimSpace(accountID)
	tenantSlug = strings.TrimSpace(tenantSlug)
	tenantName = strings.TrimSpace(tenantName)
	actor = strings.TrimSpace(actor)
	if accountID == "" {
		return TenantRow{}, PortalMembership{}, fmt.Errorf("account id is required")
	}
	if tenantName == "" {
		return TenantRow{}, PortalMembership{}, fmt.Errorf("workspace name is required")
	}
	var tenant TenantRow
	var membership PortalMembership
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if tenantSlug == "" {
			generated, err := generateWorkspaceSlug("ws_")
			if err != nil {
				return err
			}
			tenantSlug = generated
		}
		for attempt := 0; attempt < 5; attempt++ {
			var existing int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenant_memberships WHERE tenant_slug = $1 AND account_id = $2 AND revoked_at IS NULL`, tenantSlug, accountID).Scan(&existing); err != nil {
				return err
			}
			if existing > 0 {
				return fmt.Errorf("account already belongs to workspace %q", tenantSlug)
			}
			err := tx.QueryRow(ctx, `
INSERT INTO tenants (slug, name) VALUES ($1, $2)
RETURNING id::text, workspace_id, slug, name, kind, COALESCE(personal_owner_account_id::text, ''), lifecycle_status,
          lifecycle_changed_at, lifecycle_changed_by, lifecycle_reason,
          billing_plan, max_environments, max_state_resources, max_environment_resources,
          COALESCE(stripe_customer_id,''), COALESCE(stripe_subscription_id,''), COALESCE(stripe_subscription_status,''),
          COALESCE(stripe_price_id,''), stripe_current_period_end`,
				tenantSlug, tenantName,
			).Scan(
				&tenant.ID, &tenant.WorkspaceID, &tenant.Slug, &tenant.Name, &tenant.Kind, &tenant.PersonalOwnerAccountID, &tenant.LifecycleStatus,
				&tenant.LifecycleChangedAt, &tenant.LifecycleChangedBy, &tenant.LifecycleReason,
				&tenant.BillingPlan, &tenant.MaxEnvironments, &tenant.MaxStateResources, &tenant.MaxEnvironmentResources,
				&tenant.StripeCustomerID, &tenant.StripeSubID, &tenant.StripeSubStatus, &tenant.StripePriceID, &tenant.StripePeriodEnd,
			)
			if err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.TrimSpace(pgErr.ConstraintName) == "tenants_slug_key" {
					generated, genErr := generateWorkspaceSlug("ws_")
					if genErr != nil {
						return genErr
					}
					tenantSlug = generated
					continue
				}
				return err
			}
			break
		}
		if tenant.ID == "" {
			return fmt.Errorf("could not allocate workspace id")
		}
		if _, err := tx.Exec(ctx, `
UPDATE tenants
SET billing_plan = $2,
    max_environments = $3,
    max_state_resources = $4,
    max_environment_resources = $5
WHERE slug = $1`,
			tenant.Slug, StarterBillingPlan, StarterMaxEnvironments, StarterMaxStateResources, StarterMaxEnvironmentResources,
		); err != nil {
			return err
		}
		tenant.BillingPlan = StarterBillingPlan
		tenant.MaxEnvironments = StarterMaxEnvironments
		tenant.MaxStateResources = StarterMaxStateResources
		tenant.MaxEnvironmentResources = StarterMaxEnvironmentResources
		if autoDefaultEnv {
			if _, err := ensureDefaultEnvironmentWithQuerier(ctx, tx, tenant.ID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `
INSERT INTO tenant_memberships (tenant_slug, account_id, role)
VALUES ($1,$2,'owner')
RETURNING tenant_slug, role, created_at
`, tenantSlug, accountID).Scan(&membership.TenantSlug, &membership.Role, &membership.CreatedAt); err != nil {
			return err
		}
		membership.TenantName = tenantName
		membership.Kind = "organization"
		if actor != "" {
			_ = insertControlEvent(ctx, tx, "portal_tenant_create", tenant.ID, actor, map[string]any{
				"tenant_slug": tenantSlug,
				"tenant_name": tenantName,
			})
		}
		return nil
	})
	return tenant, membership, err
}

func personalWorkspaceSlug(email, accountID string) string {
	if slug, err := generateWorkspaceSlug("ws_"); err == nil {
		return slug
	}
	suffix := strings.TrimSpace(accountID)
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	if suffix == "" {
		suffix = "fallback"
	}
	return "ws_" + suffix
}

func (s *Store) CreatePersonalWorkspaceForPortalAccount(ctx context.Context, accountID, email, actor string, autoDefaultEnv bool) (TenantRow, PortalMembership, error) {
	accountID = strings.TrimSpace(accountID)
	email = strings.ToLower(strings.TrimSpace(email))
	actor = strings.TrimSpace(actor)
	if accountID == "" || email == "" {
		return TenantRow{}, PortalMembership{}, fmt.Errorf("account id and email are required")
	}
	var tenant TenantRow
	var membership PortalMembership
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
SELECT id::text, workspace_id, slug, name, kind, COALESCE(personal_owner_account_id::text, ''), lifecycle_status,
       lifecycle_changed_at, lifecycle_changed_by, lifecycle_reason,
       billing_plan, max_environments, max_state_resources, max_environment_resources,
       COALESCE(stripe_customer_id,''), COALESCE(stripe_subscription_id,''), COALESCE(stripe_subscription_status,''),
       COALESCE(stripe_price_id,''), stripe_current_period_end
FROM tenants
WHERE personal_owner_account_id = $1
LIMIT 1`, accountID).Scan(
			&tenant.ID, &tenant.WorkspaceID, &tenant.Slug, &tenant.Name, &tenant.Kind, &tenant.PersonalOwnerAccountID, &tenant.LifecycleStatus,
			&tenant.LifecycleChangedAt, &tenant.LifecycleChangedBy, &tenant.LifecycleReason,
			&tenant.BillingPlan, &tenant.MaxEnvironments, &tenant.MaxStateResources, &tenant.MaxEnvironmentResources,
			&tenant.StripeCustomerID, &tenant.StripeSubID, &tenant.StripeSubStatus, &tenant.StripePriceID, &tenant.StripePeriodEnd,
		); err == nil {
			if err := tx.QueryRow(ctx, `
SELECT tenant_slug, role, created_at
FROM tenant_memberships
WHERE tenant_slug = $1 AND account_id = $2 AND revoked_at IS NULL`,
				tenant.Slug, accountID,
			).Scan(&membership.TenantSlug, &membership.Role, &membership.CreatedAt); err == nil {
				membership.TenantName = tenant.Name
				membership.Kind = tenant.Kind
				return nil
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		name := "Personal workspace"
		slug := personalWorkspaceSlug(email, accountID)
		if err := tx.QueryRow(ctx, `
INSERT INTO tenants (slug, name, kind, personal_owner_account_id)
VALUES ($1, $2, 'personal', $3)
RETURNING id::text, workspace_id, slug, name, kind, COALESCE(personal_owner_account_id::text, ''), lifecycle_status,
          lifecycle_changed_at, lifecycle_changed_by, lifecycle_reason,
          billing_plan, max_environments, max_state_resources, max_environment_resources,
          COALESCE(stripe_customer_id,''), COALESCE(stripe_subscription_id,''), COALESCE(stripe_subscription_status,''),
          COALESCE(stripe_price_id,''), stripe_current_period_end`,
			slug, name, accountID,
		).Scan(
			&tenant.ID, &tenant.WorkspaceID, &tenant.Slug, &tenant.Name, &tenant.Kind, &tenant.PersonalOwnerAccountID, &tenant.LifecycleStatus,
			&tenant.LifecycleChangedAt, &tenant.LifecycleChangedBy, &tenant.LifecycleReason,
			&tenant.BillingPlan, &tenant.MaxEnvironments, &tenant.MaxStateResources, &tenant.MaxEnvironmentResources,
			&tenant.StripeCustomerID, &tenant.StripeSubID, &tenant.StripeSubStatus, &tenant.StripePriceID, &tenant.StripePeriodEnd,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE tenants
SET billing_plan = $2,
    max_environments = $3,
    max_state_resources = $4,
    max_environment_resources = $5
WHERE slug = $1`,
			tenant.Slug, StarterBillingPlan, StarterMaxEnvironments, StarterMaxStateResources, StarterMaxEnvironmentResources,
		); err != nil {
			return err
		}
		tenant.BillingPlan = StarterBillingPlan
		tenant.MaxEnvironments = StarterMaxEnvironments
		tenant.MaxStateResources = StarterMaxStateResources
		tenant.MaxEnvironmentResources = StarterMaxEnvironmentResources
		if autoDefaultEnv {
			if _, err := ensureDefaultEnvironmentWithQuerier(ctx, tx, tenant.ID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `
INSERT INTO tenant_memberships (tenant_slug, account_id, role)
VALUES ($1,$2,'owner')
RETURNING tenant_slug, role, created_at
`, tenant.Slug, accountID).Scan(&membership.TenantSlug, &membership.Role, &membership.CreatedAt); err != nil {
			return err
		}
		membership.TenantName = tenant.Name
		membership.Kind = "personal"
		if actor != "" {
			_ = insertControlEvent(ctx, tx, "portal_personal_workspace_create", tenant.ID, actor, map[string]any{
				"tenant_slug": tenant.Slug,
				"account_id":  accountID,
			})
		}
		return nil
	})
	return tenant, membership, err
}

func (s *Store) SetPortalUserRole(ctx context.Context, tenantSlug, accountID string, role string, actor string, actorRole string) error {
	tenantSlug = strings.TrimSpace(tenantSlug)
	accountID = strings.TrimSpace(accountID)
	role = strings.ToLower(strings.TrimSpace(role))
	actor = strings.TrimSpace(actor)
	actorRole = strings.ToLower(strings.TrimSpace(actorRole))
	if tenantSlug == "" {
		return fmt.Errorf("tenant slug is required")
	}
	if accountID == "" {
		return fmt.Errorf("user id is required")
	}
	switch role {
	case "owner", "tenant_admin", "billing_admin", "member":
	default:
		return fmt.Errorf("invalid role %q", role)
	}
	if actorRole != "owner" && actorRole != "tenant_admin" && actorRole != "platform_admin" {
		return fmt.Errorf("insufficient role")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var oldRole string
		err := tx.QueryRow(ctx, `
SELECT role
FROM tenant_memberships
WHERE tenant_slug = $1
  AND account_id = $2
  AND revoked_at IS NULL
FOR UPDATE`, tenantSlug, accountID).Scan(&oldRole)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("portal user membership not found")
		}
		if err != nil {
			return err
		}
		if actorRole == "tenant_admin" {
			if role == "owner" {
				return fmt.Errorf("tenant admin cannot grant owner role")
			}
			if strings.EqualFold(strings.TrimSpace(oldRole), "owner") {
				return fmt.Errorf("tenant admin cannot change owner role")
			}
		}

		if strings.EqualFold(strings.TrimSpace(oldRole), "owner") && role != "owner" {
			var owners int
			if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM tenant_memberships
WHERE tenant_slug = $1
  AND role = 'owner'
  AND revoked_at IS NULL`, tenantSlug).Scan(&owners); err != nil {
				return err
			}
			if owners <= 1 {
				return fmt.Errorf("cannot demote the last owner")
			}
		}

		tag, err := tx.Exec(ctx, `
UPDATE tenant_memberships
SET role = $3,
    updated_at = now()
WHERE tenant_slug = $1
  AND account_id = $2
  AND revoked_at IS NULL`, tenantSlug, accountID, role)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("portal user membership not found")
		}
		var tenantID string
		_ = tx.QueryRow(ctx, `SELECT id::text FROM tenants WHERE slug = $1`, tenantSlug).Scan(&tenantID)
		_ = insertControlEvent(ctx, tx, "portal_user_role_update", tenantID, actor, map[string]any{
			"user_id":     accountID,
			"tenant_slug": tenantSlug,
			"from":        oldRole,
			"to":          role,
		})
		return nil
	})
}

func (s *Store) RemovePortalUserFromTenant(ctx context.Context, tenantSlug, accountID string, actor string, actorRole string) error {
	tenantSlug = strings.TrimSpace(tenantSlug)
	accountID = strings.TrimSpace(accountID)
	actor = strings.TrimSpace(actor)
	actorRole = strings.ToLower(strings.TrimSpace(actorRole))
	if tenantSlug == "" {
		return fmt.Errorf("tenant slug is required")
	}
	if accountID == "" {
		return fmt.Errorf("user id is required")
	}
	if actorRole != "owner" && actorRole != "tenant_admin" && actorRole != "platform_admin" {
		return fmt.Errorf("insufficient role")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var oldRole string
		err := tx.QueryRow(ctx, `
SELECT role
FROM tenant_memberships
WHERE tenant_slug = $1
  AND account_id = $2
  AND revoked_at IS NULL
FOR UPDATE`, tenantSlug, accountID).Scan(&oldRole)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("portal user membership not found")
		}
		if err != nil {
			return err
		}
		if actorRole == "tenant_admin" && strings.EqualFold(strings.TrimSpace(oldRole), "owner") {
			return fmt.Errorf("tenant admin cannot remove an owner")
		}

		if strings.EqualFold(strings.TrimSpace(oldRole), "owner") {
			var owners int
			if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM tenant_memberships
WHERE tenant_slug = $1
  AND role = 'owner'
  AND revoked_at IS NULL`, tenantSlug).Scan(&owners); err != nil {
				return err
			}
			if owners <= 1 {
				return fmt.Errorf("cannot remove the last owner")
			}
		}

		tag, err := tx.Exec(ctx, `
UPDATE tenant_memberships
SET revoked_at = now(),
    updated_at = now()
WHERE tenant_slug = $1
  AND account_id = $2
  AND revoked_at IS NULL`, tenantSlug, accountID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("portal user membership not found")
		}
		var tenantID string
		_ = tx.QueryRow(ctx, `SELECT id::text FROM tenants WHERE slug = $1`, tenantSlug).Scan(&tenantID)
		_ = insertControlEvent(ctx, tx, "portal_user_membership_remove", tenantID, actor, map[string]any{
			"user_id":     accountID,
			"tenant_slug": tenantSlug,
			"role":        oldRole,
		})
		return nil
	})
}

func (s *Store) LeavePortalTenant(ctx context.Context, tenantSlug, accountID, actor string) (string, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	accountID = strings.TrimSpace(accountID)
	actor = strings.TrimSpace(actor)
	if tenantSlug == "" {
		return "", fmt.Errorf("tenant slug is required")
	}
	if accountID == "" {
		return "", fmt.Errorf("account id is required")
	}

	var nextTenant string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			role     string
			kind     string
			tenantID string
		)
		err := tx.QueryRow(ctx, `
SELECT tm.role, t.kind, t.id::text
FROM tenant_memberships tm
JOIN tenants t ON t.slug = tm.tenant_slug
WHERE tm.tenant_slug = $1
  AND tm.account_id = $2
  AND tm.revoked_at IS NULL
FOR UPDATE`, tenantSlug, accountID).Scan(&role, &kind, &tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("portal user membership not found")
		}
		if err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(kind), "personal") {
			return fmt.Errorf("personal workspace cannot be left")
		}
		if strings.EqualFold(strings.TrimSpace(role), "owner") {
			var owners int
			if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM tenant_memberships
WHERE tenant_slug = $1
  AND role = 'owner'
  AND revoked_at IS NULL`, tenantSlug).Scan(&owners); err != nil {
				return err
			}
			if owners <= 1 {
				return fmt.Errorf("cannot leave the last owner workspace")
			}
		}

		tag, err := tx.Exec(ctx, `
UPDATE tenant_memberships
SET revoked_at = now(),
    updated_at = now()
WHERE tenant_slug = $1
  AND account_id = $2
  AND revoked_at IS NULL`, tenantSlug, accountID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("portal user membership not found")
		}

		_ = tx.QueryRow(ctx, `
SELECT tm.tenant_slug
FROM tenant_memberships tm
JOIN tenants t ON t.slug = tm.tenant_slug
WHERE tm.account_id = $1
  AND tm.revoked_at IS NULL
ORDER BY CASE tm.role WHEN 'owner' THEN 0 WHEN 'tenant_admin' THEN 1 WHEN 'billing_admin' THEN 2 ELSE 3 END,
         tm.created_at ASC
LIMIT 1`, accountID).Scan(&nextTenant)

		_ = insertControlEvent(ctx, tx, "portal_user_membership_leave", tenantID, actor, map[string]any{
			"user_id":     accountID,
			"tenant_slug": tenantSlug,
			"role":        role,
		})
		return nil
	})
	return nextTenant, err
}
