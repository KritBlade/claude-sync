package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// A conversation in Antigravity's current format is one SQLite file (conversations/<id>.db). Paths sit inside
// its protobuf blobs: the workspace in trajectory_metadata_blob decides where the agent runs commands, and
// the steps name uploaded images and the files the agent worked on.

func isConversationDatabase(relativePath string) bool {
	return strings.HasPrefix(relativePath, "antigravity/conversations/") && strings.HasSuffix(relativePath, ".db")
}

// openDatabaseReadOnly opens a conversation for reading without writing to it, safe on the live file even
// while Antigravity has it open.
func openDatabaseReadOnly(path string) (*sql.DB, error) {
	return openDatabaseWithMode(path, "mode=ro&")
}

func openDatabase(path string) (*sql.DB, error) {
	return openDatabaseWithMode(path, "")
}

func openDatabaseWithMode(path, mode string) (*sql.DB, error) {
	database, err := sql.Open("sqlite", "file:"+path+"?"+mode+"_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	database.SetMaxOpenConns(1)
	return database, nil
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// snapshotDatabase writes a consistent copy of a conversation, including changes still in its WAL file, to
// destinationPath, which must not exist. It is safe while Antigravity has the conversation open.
func snapshotDatabase(sourcePath, destinationPath string) error {
	database, err := openDatabaseReadOnly(sourcePath)
	if err != nil {
		return err
	}
	defer closeOrWarn(database, sourcePath)
	if _, err := database.Exec("VACUUM INTO ?", destinationPath); err != nil {
		return fmt.Errorf("snapshot %s: %w", sourcePath, err)
	}
	return nil
}

// rowQuerier is a database or a transaction, so reads inside a conversion use the transaction's connection.
type rowQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func listTables(database rowQuerier) ([]string, error) {
	rows, err := database.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func listColumns(database rowQuerier, table string) ([]string, error) {
	rows, err := database.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// readAllRows returns each row of table as its rowid followed by its column values.
func readAllRows(database rowQuerier, table string, columns []string) ([][]any, error) {
	quotedColumns := make([]string, len(columns))
	for i, column := range columns {
		quotedColumns[i] = quoteIdentifier(column)
	}
	query := "SELECT rowid, " + strings.Join(quotedColumns, ", ") + " FROM " + quoteIdentifier(table) + " ORDER BY rowid"
	rows, err := database.Query(query)
	if err != nil {
		return nil, fmt.Errorf("read table %s: %w", table, err)
	}
	defer rows.Close()
	var result [][]any
	for rows.Next() {
		values := make([]any, len(columns)+1)
		pointers := make([]any, len(values))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		result = append(result, values)
	}
	return result, rows.Err()
}

type databaseConversionReport struct {
	changedValues   int
	unhandledFields int
}

// convertDatabaseFile rewrites paths in every text and blob value of the SQLite file at path, in place, then
// checks the file's integrity and leaves it in rollback-journal mode so it is a single self-contained file.
func convertDatabaseFile(path string, convertText func([]byte) []byte, hasCandidate func([]byte) bool) (databaseConversionReport, error) {
	report := databaseConversionReport{}
	database, err := openDatabase(path)
	if err != nil {
		return report, err
	}
	defer closeOrWarn(database, path)
	tables, err := listTables(database)
	if err != nil {
		return report, fmt.Errorf("list tables of %s: %w", path, err)
	}
	blobConverter := &protobufTextConverter{convertText: convertText, hasCandidate: hasCandidate}
	transaction, err := database.Begin()
	if err != nil {
		return report, err
	}
	for _, table := range tables {
		columns, err := listColumns(transaction, table)
		if err != nil {
			rollbackOrWarn(transaction, path)
			return report, fmt.Errorf("list columns of %s: %w", table, err)
		}
		rows, err := readAllRows(transaction, table, columns)
		if err != nil {
			rollbackOrWarn(transaction, path)
			return report, err
		}
		for _, row := range rows {
			for columnIndex, column := range columns {
				newValue, isChanged, err := convertStoredValue(row[columnIndex+1], blobConverter, convertText, hasCandidate)
				if err != nil {
					rollbackOrWarn(transaction, path)
					return report, fmt.Errorf("%s.%s row %v: %w", table, column, row[0], err)
				}
				if !isChanged {
					continue
				}
				update := "UPDATE " + quoteIdentifier(table) + " SET " + quoteIdentifier(column) + " = ? WHERE rowid = ?"
				if _, err := transaction.Exec(update, newValue, row[0]); err != nil {
					rollbackOrWarn(transaction, path)
					return report, fmt.Errorf("update %s.%s: %w", table, column, err)
				}
				report.changedValues++
			}
		}
	}
	if err := transaction.Commit(); err != nil {
		return report, err
	}
	report.unhandledFields = blobConverter.unhandledFields
	var integrity string
	if err := database.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return report, fmt.Errorf("integrity check of %s: %w", path, err)
	}
	if integrity != "ok" {
		return report, fmt.Errorf("integrity check of %s failed: %s", path, integrity)
	}
	if _, err := database.Exec("PRAGMA journal_mode = DELETE"); err != nil {
		return report, fmt.Errorf("set journal mode of %s: %w", path, err)
	}
	return report, nil
}

func convertStoredValue(value any, blobConverter *protobufTextConverter, convertText func([]byte) []byte, hasCandidate func([]byte) bool) (any, bool, error) {
	switch typed := value.(type) {
	case []byte:
		if !hasCandidate(typed) {
			return nil, false, nil
		}
		converted, err := blobConverter.convertBlob(typed)
		if err != nil {
			return nil, false, err
		}
		return converted, string(converted) != string(typed), nil
	case string:
		if !hasCandidate([]byte(typed)) {
			return nil, false, nil
		}
		converted := string(convertText([]byte(typed)))
		return converted, converted != typed, nil
	}
	return nil, false, nil
}

// databaseContentHash hashes the rows of every table, so it changes only when the conversation's content
// does. A file-level hash would also change when Antigravity merely opens the file and switches its journal
// mode, and would turn every such open into a sync conflict.
func databaseContentHash(path string) (string, error) {
	database, err := openDatabaseReadOnly(path)
	if err != nil {
		return "", err
	}
	defer closeOrWarn(database, path)
	tables, err := listTables(database)
	if err != nil {
		return "", fmt.Errorf("list tables of %s: %w", path, err)
	}
	sort.Strings(tables)
	digest := sha256.New()
	for _, table := range tables {
		columns, err := listColumns(database, table)
		if err != nil {
			return "", err
		}
		rows, err := readAllRows(database, table, columns)
		if err != nil {
			return "", err
		}
		writeHashedValue(digest, "table:"+table)
		for _, row := range rows {
			for _, value := range row {
				writeHashedValue(digest, value)
			}
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

type hashWriter interface{ Write([]byte) (int, error) }

// writeHashedValue writes a type tag, a length and the value, so different rows can never hash alike.
func writeHashedValue(digest hashWriter, value any) {
	var encoded []byte
	var tag byte
	switch typed := value.(type) {
	case nil:
		tag = 'n'
	case int64:
		tag = 'i'
		encoded = binary.BigEndian.AppendUint64(nil, uint64(typed))
	case float64:
		tag = 'f'
		encoded = binary.BigEndian.AppendUint64(nil, math.Float64bits(typed))
	case string:
		tag = 's'
		encoded = []byte(typed)
	case []byte:
		tag = 'b'
		encoded = typed
	case bool:
		tag = 'o'
		if typed {
			encoded = []byte{1}
		}
	default:
		tag = '?'
		encoded = []byte(fmt.Sprint(typed))
	}
	header := binary.BigEndian.AppendUint64([]byte{tag}, uint64(len(encoded)))
	_, _ = digest.Write(header) // EXPECTED: hash.Hash writes never fail
	_, _ = digest.Write(encoded)
}

// rollbackOrWarn undoes a failed conversion; the caller returns the error that caused it.
func rollbackOrWarn(transaction *sql.Tx, path string) {
	if err := transaction.Rollback(); err != nil {
		warnf("roll back conversion of %s: %v", path, err)
	}
}

func closeOrWarn(database *sql.DB, path string) {
	if err := database.Close(); err != nil {
		warnf("close %s: %v", path, err)
	}
}
