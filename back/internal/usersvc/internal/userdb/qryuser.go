package userdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/OpenEugene/openboard/back/internal/altr"
	"github.com/OpenEugene/openboard/back/internal/pb"
	"github.com/codemodus/uidgen"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

type cx = context.Context

var (
	csvStr = altr.CSVFromStrings
	lim    = altr.LimitUint32
	asTS   = altr.Timestamp
)

func parseOrUID(ug *uidgen.UIDGen, sid string) (uidgen.UID, bool) {
	if sid == "" {
		return ug.UID(), true
	}
	return ug.Parse(sid)
}

func (s *UserDB) upsertUser(ctx cx, sid string, x *pb.AddUserReq, y *pb.UserResp) error {
	id, ok := parseOrUID(s.ug, sid)
	if !ok {
		return fmt.Errorf("invalid uid")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	qry := `
		INSERT INTO user (
			user_id, username, email, email_hold, altmail, altmail_hold, full_name, avatar, password
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) 
		ON DUPLICATE KEY UPDATE 
			user_id = ?,
			username = ?, 
			email = ?, 
			email_hold = ?, 
			altmail = ?, 
			altmail_hold = ?, 
			full_name = ?, 
			avatar = ?, 
			password = ?
	`
	_, err = tx.ExecContext(
		ctx,
		qry,
		&id,
		x.Username,
		x.Email,
		x.EmailHold,
		x.Altmail,
		x.AltmailHold,
		x.FullName,
		x.Avatar,
		x.Password,
		&id,
		x.Username,
		x.Email,
		x.EmailHold,
		x.Altmail,
		x.AltmailHold,
		x.FullName,
		x.Avatar,
		x.Password,
	)
	if err != nil {
		tx.Rollback()
		return err
	}

	qry = "INSERT into user_role (user_id, role_id) VALUES "
	vals, args := buildValsAndArgs(id.String(), x.RoleIds)
	_, err = tx.ExecContext(ctx, qry+vals, args...)
	if err != nil {
		tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Execute another query that will return the user fields.
	req := pb.FndUsersReq{
		RoleIds:     []string{},
		Email:       x.Email,
		EmailHold:   false,
		Altmail:     "",
		AltmailHold: false,
		Limit:       1,
		Lapse:       0,
	}
	resp := pb.UsersResp{}

	if err = s.findUsers(ctx, &req, &resp); err != nil {
		return err
	}

	if len(resp.Items) == 0 {
		return errors.New("expected user to be found, but found none")
	}

	// There is only one user (Item) expected to be found.
	y.Item = resp.Items[0]

	return nil
}

// buildValsAndArgs enables adding all the role IDs for the user ID being added.
// The values returned have bindvar pairs for each userId/roleID pair in args.
func buildValsAndArgs(uid string, rids []string) (string, []interface{}) {
	vals := "(?, ?)"

	args := make([]interface{}, len(rids)*2)
	args[0] = uid
	args[1] = rids[0]

	for i, rid := range rids[1:] {
		vals += ", (?, ?)"

		args[i*2+2] = uid
		args[i*2+3] = rid
	}

	return vals, args
}

func (s *UserDB) deleteUser(ctx cx, sid string) error {
	_, err := s.db.Exec(
		"UPDATE user SET deleted_at = ? WHERE user_id = ?",
		time.Now(),
		sid,
	)
	if err != nil {
		return err
	}

	return nil
}

type userTemp struct {
	uid, username, email, altmail, fullName, avatar, rid, rolename string
	emailHold, altmailHold                                         bool
	tl, tc, tu, td, tb                                             mysql.NullTime
}

func (s *UserDB) findUsers(ctx cx, x *pb.FndUsersReq, y *pb.UsersResp) error {
	qry := `
		SELECT u.user_id, u.username, u.email, u.email_hold, u.altmail, 
			u.altmail_hold, u.full_name, u.avatar, r.role_id, r.role_name, 
			u.last_login, u.created_at, u.updated_at, u.deleted_at, u.blocked_at 
		FROM (
			SELECT user_id, username, email, email_hold, altmail, altmail_hold, 
				full_name, avatar, last_login, created_at, updated_at, deleted_at, 
				blocked_at 
			FROM user WHERE email = ? AND email_hold = ? 
			LIMIT ? OFFSET ?
		) u 
		LEFT JOIN user_role ur 
			ON u.user_id = ur.user_id 
		LEFT JOIN role r 
			ON r.role_id = ur.role_id
	`

	rows, err := s.db.Query(qry, x.Email, x.EmailHold, x.Limit, x.Lapse)
	defer rows.Close()
	if err != nil {
		return err
	}

	temps := []userTemp{}

	for rows.Next() {
		u := userTemp{}

		err := rows.Scan(
			&u.uid,
			&u.username,
			&u.email,
			&u.emailHold,
			&u.altmail,
			&u.altmailHold,
			&u.fullName,
			&u.avatar,
			&u.rid,
			&u.rolename,
			&u.tl,
			&u.tc,
			&u.tu,
			&u.td,
			&u.tb,
		)
		if err != nil {
			return err
		}

		temps = append(temps, u)
	}
	if err = rows.Err(); err != nil {
		return err
	}

	users := squashUsers(temps)

	for _, u := range users {
		y.Items = append(y.Items, &u)
	}

	err = s.db.QueryRow(
		"SELECT COUNT(*) FROM user WHERE email = ? AND email_hold = ?",
		x.Email,
		x.EmailHold,
	).Scan(&y.Total)
	if err != nil {
		return err
	}

	return nil
}

// squashUsers combines user information so there are no duplicate user IDs in slice.
func squashUsers(uts []userTemp) []pb.User {
	var users []pb.User

	for _, ut := range uts {
		i := fndUserIndex(ut, users)

		if i == -1 {
			usr := convertUserTemp(ut)
			users = append(users, usr)
		} else {
			r := pb.RoleResp{
				Id:   ut.rid,
				Name: ut.rolename,
			}

			users[i].Roles = append(users[i].Roles, &r)
		}
	}

	return users
}

// userIndex gets the index of a user in []pb.User, or -1 if not found.
func fndUserIndex(ut userTemp, users []pb.User) int {
	for i, u := range users {
		if u.Id == ut.uid {
			return i
		}
	}

	return -1
}

// convertUserTemp transfers information from userTemp to pb.User.
func convertUserTemp(ut userTemp) pb.User {
	var u pb.User

	r := pb.RoleResp{
		Id:   ut.rid,
		Name: ut.rolename,
	}

	u.Id = ut.uid
	u.Username = ut.username
	u.Email = ut.email
	u.EmailHold = ut.emailHold
	u.Altmail = ut.altmail
	u.AltmailHold = ut.altmailHold
	u.FullName = ut.fullName
	u.Avatar = ut.avatar
	u.Roles = append(u.Roles, &r)
	u.LastLogin = asTS(ut.tl.Time, ut.tl.Valid)
	u.Created = asTS(ut.tc.Time, ut.tc.Valid)
	u.Updated = asTS(ut.tu.Time, ut.tu.Valid)
	u.Deleted = asTS(ut.td.Time, ut.td.Valid)

	return u
}

func (s *UserDB) upsertRole(ctx cx, sid string, x *pb.AddRoleReq, y *pb.RoleResp) error {
	id, ok := parseOrUID(s.ug, sid)
	if !ok {
		return fmt.Errorf("invalid uid")
	}

	qry := `
		INSERT INTO role (role_id, role_name) 
		VALUES (?, ?) 
		ON DUPLICATE KEY UPDATE role_id = ?, role_name = ?
	`

	_, err := s.db.Exec(qry, &id, x.Name, &id, x.Name)
	if err != nil {
		return err
	}

	// Execute another query that will return the role fields.
	req := pb.FndRolesReq{
		RoleIds:   []string{},
		RoleNames: []string{x.Name},
		Limit:     1,
		Lapse:     0,
	}
	resp := pb.RolesResp{}
	if err = s.findRoles(ctx, &req, &resp); err != nil {
		return err
	}

	if len(resp.Items) == 0 {
		return errors.New("upserted role not found")
	}

	// There is only one role (Item) expected to be found.
	y.Id = resp.Items[0].Id
	y.Name = resp.Items[0].Name

	return nil
}

func (s *UserDB) findRoles(ctx cx, x *pb.FndRolesReq, y *pb.RolesResp) error {
	req := *x
	roleIds := req.RoleIds
	roleNames := req.RoleNames

	if len(roleIds) == 0 {
		roleIds = []string{""}
	}
	if len(roleNames) == 0 {
		roleNames = []string{""}
	}

	qry := `
		SELECT role_id, role_name
		FROM role
		WHERE role_id IN (?) OR role_name IN (?) LIMIT ? OFFSET ?;
	`

	query, args, err := sqlx.In(qry, roleIds, roleNames, x.Limit, x.Lapse)
	if err != nil {
		return err
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		r := pb.RoleResp{}

		if err := rows.Scan(&r.Id, &r.Name); err != nil {
			return err
		}

		y.Items = append(y.Items, &r)
	}
	if err = rows.Err(); err != nil {
		return err
	}

	qry = "SELECT COUNT(*) FROM role WHERE role_id IN (?) OR role_name IN (?);"

	query, args, err = sqlx.In(qry, roleIds, roleNames)
	if err != nil {
		return err
	}

	err = s.db.QueryRow(query, args...).Scan(&y.Total)
	if err != nil {
		return err
	}

	return nil
}
