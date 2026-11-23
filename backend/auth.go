package main

import (
	"database/sql"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

func initializeAdministrator(db *sql.DB, user, password string) (string, error) {
	var storedUser string
	var hash string
	err := db.QueryRow(`SELECT username, password_hash FROM administrators WHERE id=1`).Scan(&storedUser, &hash)
	if err == nil {
		return storedUser, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("load administrator credentials: %w", err)
	}
	if user == "" || password == "" {
		return "", errors.New("administrator credentials are missing: set ORCA_ADMIN_USER and ORCA_ADMIN_PASSWORD to initialize the database")
	}
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash initial administrator password: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO administrators(id, username, password_hash) VALUES(1, ?, ?)`, user, string(hashedPassword)); err != nil {
		return "", fmt.Errorf("initialize administrator: %w", err)
	}
	if err = tx.QueryRow(`SELECT username FROM administrators WHERE id=1`).Scan(&storedUser); err != nil {
		return "", fmt.Errorf("load initialized administrator: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return storedUser, nil
}
