// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package database

import (
	"cambio/pkg/logging"
	"cambio/pkg/model"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	// InsertInfectionsBatchSize is the maximum number of infections that can be inserted at once.
	InsertInfectionsBatchSize = 500
)

// InfectionIterator iterates over a set of infections.
type InfectionIterator interface {
	// Next returns an infection and a flag indicating if the iterator is done (the infection will be nil when done==true).
	Next() (infection *model.Infection, done bool, err error)
	// Cursor returns a string that can be passed as LastCursor in FetchInfectionsCriteria when generating another iterator.
	Cursor() (string, error)
	// Close should be called when done iterating.
	Close() error
}

// IterateInfectionsCriteria is criteria to iterate infections.
type IterateInfectionsCriteria struct {
	IncludeRegions []string
	SinceTimestamp time.Time
	UntilTimestamp time.Time
	LastCursor     string

	// OnlyLocalProvenance indicates that only infections with LocalProvenance=true will be returned.
	OnlyLocalProvenance bool
}

// IterateInfections returns an iterator for infections meeting the criteria.
func IterateInfections(ctx context.Context, criteria IterateInfectionsCriteria) (InfectionIterator, error) {
	conn, err := Connection(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to obtain database connection: %v", err)
	}

	offset := 0
	if criteria.LastCursor != "" {
		offsetStr, err := decodeCursor(criteria.LastCursor)
		if err != nil {
			return nil, fmt.Errorf("unable to decode cursor: %v", err)
		}
		offset, err = strconv.Atoi(offsetStr)
		if err != nil {
			return nil, fmt.Errorf("unable to decode cursor %v", err)
		}
	}

	query, args, err := generateQuery(criteria)
	if err != nil {
		return nil, fmt.Errorf("generating query: %v", err)
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	return &postgresInfectionIterator{rows: rows, offset: offset}, nil
}

type postgresInfectionIterator struct {
	rows   *sql.Rows
	offset int
}

func (i *postgresInfectionIterator) Next() (*model.Infection, bool, error) {
	if i.rows == nil {
		return nil, true, nil
	}

	if !i.rows.Next() {
		return nil, true, nil
	}

	var m model.Infection
	var encodedExposureKey string
	if err := i.rows.Scan(&encodedExposureKey, &m.DiagnosisStatus, &m.AppPackageName, pq.Array(&m.Regions), &m.IntervalNumber,
		&m.IntervalCount, &m.CreatedAt, &m.LocalProvenance, &m.VerificationAuthorityName, &m.FederationSyncID); err != nil {
		return nil, false, err
	}

	var err error
	m.ExposureKey, err = decodeExposureKey(encodedExposureKey)
	if err != nil {
		return nil, false, err
	}
	i.offset++

	return &m, false, nil
}

func (i *postgresInfectionIterator) Cursor() (string, error) {
	// TODO: this is a pretty weak cursor solution, but not too bad since we'll typcially have queries ahead of the wipeout
	// and before the current ingestion window, and those should be stable.
	return encodeCursor(strconv.Itoa(i.offset)), nil
}

func (i *postgresInfectionIterator) Close() error {
	if i.rows != nil {
		i.rows.Close()
	}
	return nil
}

func generateQuery(criteria IterateInfectionsCriteria) (string, []interface{}, error) {
	q := `
		SELECT
			exposure_key, diagnosis_status, app_package_name, regions, interval_number, interval_count,
			created_at, local_provenance, verification_authority_name, sync_id
		FROM Infection
		WHERE 1=1
		`
	var args []interface{}

	if len(criteria.IncludeRegions) == 1 {
		args = append(args, pq.Array(criteria.IncludeRegions))
		q += fmt.Sprintf(" AND (regions && $%d)", len(args)) // Operation "&&" means "array overlaps / intersects"
	}

	if !criteria.SinceTimestamp.IsZero() {
		args = append(args, criteria.SinceTimestamp)
		q += fmt.Sprintf(" AND created_at > $%d", len(args))
	}

	if !criteria.UntilTimestamp.IsZero() {
		args = append(args, criteria.UntilTimestamp)
		q += fmt.Sprintf(" AND created_at <= $%d", len(args))
	}

	if criteria.OnlyLocalProvenance {
		args = append(args, true)
		q += fmt.Sprintf(" AND local_provenance = $%d", len(args))
	}

	q += " ORDER BY created_at"

	if criteria.LastCursor != "" {
		decoded, err := decodeCursor(criteria.LastCursor)
		if err != nil {
			return "", nil, err
		}
		args = append(args, decoded)
		q += fmt.Sprintf(" OFFSET $%d", len(args))
	}
	q = strings.ReplaceAll(q, "\n", " ")

	return q, args, nil
}

// InsertInfections inserts a set of infections.
func InsertInfections(ctx context.Context, infections []*model.Infection) (err error) {
	conn, err := Connection(ctx)
	if err != nil {
		return fmt.Errorf("unable to obtain database connection: %v", err)
	}
	logger := logging.FromContext(ctx)

	commit := false
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if commit {
			if err1 := tx.Commit(); err1 != nil {
				err = err1
			}
		} else {
			if err1 := tx.Rollback(); err1 != nil {
				err = err1
			} else {
				logger.Debugf("Rolling back.")
			}
		}
	}()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO Infection
		  (exposure_key, diagnosis_status, app_package_name, regions, interval_number, interval_count, 
		  created_at, local_provenance, verification_authority_name, sync_id)
		VALUES
		  ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (exposure_key) DO NOTHING
		`)
	if err != nil {
		return fmt.Errorf("preparing insert statment: %v", err)
	}

	for _, inf := range infections {
		_, err := stmt.ExecContext(ctx, encodeExposureKey(inf.ExposureKey), inf.DiagnosisStatus, inf.AppPackageName, pq.Array(inf.Regions), inf.IntervalNumber, inf.IntervalCount,
			inf.CreatedAt, inf.LocalProvenance, inf.VerificationAuthorityName, inf.FederationSyncID)
		if err != nil {
			return fmt.Errorf("inserting infection: %v", err)
		}
	}
	if err := stmt.Close(); err != nil {
		return fmt.Errorf("closing prepared statement: %v", err)
	}

	commit = true
	return nil
}

// DeleteInfections deletes infections created before "before" date. Returns the number of records deleted.
func DeleteInfections(ctx context.Context, before time.Time) (count int64, err error) {
	conn, err := Connection(ctx)
	if err != nil {
		return 0, fmt.Errorf("unable to obtain database connection: %v", err)
	}
	logger := logging.FromContext(ctx)

	commit := false
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("starting transaction: %v", err)
	}
	defer func() {
		if commit {
			if err1 := tx.Commit(); err1 != nil {
				err = fmt.Errorf("failed to commit: %v", err1)
			}
		} else {
			if err1 := tx.Rollback(); err1 != nil {
				err = fmt.Errorf("failed to rollback: %v", err1)
			} else {
				logger.Debugf("Rolling back.")
			}
		}
	}()

	result, err := tx.ExecContext(ctx, `
			DELETE FROM Infection
			WHERE
				created_at < $1
			`, before)
	if err != nil {
		return 0, fmt.Errorf("deleting infections: %v", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	commit = true
	return rows, nil
}

func encodeCursor(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func decodeCursor(encoded string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding cursor: %v", err)
	}
	return string(b), nil
}

func encodeExposureKey(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func decodeExposureKey(encoded string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(encoded)
}