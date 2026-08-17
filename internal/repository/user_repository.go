package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/internal/models"
)

// UserRepository provides SQL data access for users.
type UserRepository struct {
	db *database.DB
}

func NewUserRepository(db *database.DB) *UserRepository {
	return &UserRepository{db: db}
}

func (r *UserRepository) Create(ctx context.Context, u *models.User) error {
	query := `
		INSERT INTO users (tenant_id, email, password_hash, first_name, last_name, role, is_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at, updated_at`
	err := r.db.QueryRowContext(ctx, query, u.TenantID, u.Email, u.PasswordHash, u.FirstName, u.LastName, u.Role, u.IsActive).
		Scan(&u.ID, &u.CreatedAt, &u.UpdatedAt)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (r *UserRepository) GetByID(ctx context.Context, id string) (*models.User, error) {
	query := `
		SELECT id, tenant_id, email, password_hash, first_name, last_name, role, is_active, last_login_at, created_at, updated_at, deleted_at
		FROM users WHERE id = $1 AND deleted_at IS NULL`
	return r.scanOne(r.db.QueryRowContext(ctx, query, id))
}

func (r *UserRepository) GetByEmail(ctx context.Context, email string) (*models.User, error) {
	query := `
		SELECT id, tenant_id, email, password_hash, first_name, last_name, role, is_active, last_login_at, created_at, updated_at, deleted_at
		FROM users WHERE email = $1 AND deleted_at IS NULL`
	return r.scanOne(r.db.QueryRowContext(ctx, query, email))
}

func (r *UserRepository) ListByTenant(ctx context.Context, tenantID string, page, pageSize int) ([]*models.User, int64, error) {
	offset := (page - 1) * pageSize

	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `
		SELECT id, tenant_id, email, password_hash, first_name, last_name, role, is_active, last_login_at, created_at, updated_at, deleted_at
		FROM users WHERE tenant_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC LIMIT $2 OFFSET $3`
	rows, err := r.db.QueryContext(ctx, query, tenantID, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var users []*models.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		users = append(users, u)
	}
	return users, total, rows.Err()
}

func (r *UserRepository) UpdateRole(ctx context.Context, id string, role models.Role) error {
	res, err := r.db.ExecContext(ctx, `UPDATE users SET role = $1, updated_at = now() WHERE id = $2 AND deleted_at IS NULL`, role, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *UserRepository) UpdateLastLogin(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, id)
	return err
}

func (r *UserRepository) scanOne(row rowScanner) (*models.User, error) {
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func scanUser(row rowScanner) (*models.User, error) {
	var u models.User
	err := row.Scan(
		&u.ID, &u.TenantID, &u.Email, &u.PasswordHash, &u.FirstName, &u.LastName,
		&u.Role, &u.IsActive, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetByIDInTenant is the tenant-scoped read used by every authenticated
// code path. The tenant predicate lives in the SQL rather than in a Go
// comparison after the fact, so a row belonging to another tenant is never
// loaded into memory in the first place — defence in depth against a
// forgotten check further up the stack.
func (r *UserRepository) GetByIDInTenant(ctx context.Context, tenantID, id string) (*models.User, error) {
	query := `
		SELECT id, tenant_id, email, password_hash, first_name, last_name, role, is_active, last_login_at, created_at, updated_at, deleted_at
		FROM users WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`
	return r.scanOne(r.db.QueryRowContext(ctx, query, id, tenantID))
}

// UpdateRoleInTenant changes a user's role, refusing to touch rows outside
// the caller's tenant.
func (r *UserRepository) UpdateRoleInTenant(ctx context.Context, tenantID, id string, role models.Role) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE users SET role = $1, updated_at = now()
		 WHERE id = $2 AND tenant_id = $3 AND deleted_at IS NULL`,
		role, id, tenantID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountAdminsInTenant counts active admins in a tenant. Used to refuse the
// demotion of a tenant's last admin, which would otherwise leave the tenant
// permanently unable to manage itself.
func (r *UserRepository) CountAdminsInTenant(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users
		 WHERE tenant_id = $1 AND role = 'admin' AND is_active = TRUE AND deleted_at IS NULL`,
		tenantID).Scan(&n)
	return n, err
}

// SetActiveInTenant activates or deactivates a user within a tenant.
func (r *UserRepository) SetActiveInTenant(ctx context.Context, tenantID, id string, active bool) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE users SET is_active = $1, updated_at = now()
		 WHERE id = $2 AND tenant_id = $3 AND deleted_at IS NULL`,
		active, id, tenantID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
