// pmm-agent
// Copyright (C) 2018 Percona LLC
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package actions

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/AlekSi/pointer"
	"github.com/lib/pq"
	"github.com/percona/pmm/api/agentpb"
	"github.com/pkg/errors"
)

type columnInfo struct {
	Attname        string
	FormatType     string
	Substring      *string
	Attnotnull     bool
	Attcollation   *string
	Attidentity    string
	Attstorage     string
	Attstattarget  *string
	ColDescription *string
}

type indexInfo struct {
	Relname            string
	IsPrimary          bool
	IsUnique           bool
	IsClustered        bool
	IsValid            bool
	Indrelid           string
	PgGetIndexDef      *string
	PgGetConstraintDef *string
	Contype            *string
	Condeferrable      *bool
	Condeferred        *bool
	Indisreplident     *bool
	Reltablespace      int
}

type postgresqlShowCreateTableAction struct {
	id     string
	params *agentpb.StartActionRequest_PostgreSQLShowCreateTableParams
}

// NewPostgreSQLShowCreateTableAction creates PostgreSQL SHOW CREATE TABLE Action.
// This is an Action that can run `\d+ table` command analog on PostgreSQL service with given DSN.
func NewPostgreSQLShowCreateTableAction(id string, params *agentpb.StartActionRequest_PostgreSQLShowCreateTableParams) Action {
	return &postgresqlShowCreateTableAction{
		id:     id,
		params: params,
	}
}

// ID returns an Action ID.
func (a *postgresqlShowCreateTableAction) ID() string {
	return a.id
}

// Type returns an Action type.
func (a *postgresqlShowCreateTableAction) Type() string {
	return "postgresql-show-create-table"
}

// Run runs an Action and returns output and error.
func (a *postgresqlShowCreateTableAction) Run(ctx context.Context) ([]byte, error) {
	connector, err := pq.NewConnector(a.params.Dsn)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close() //nolint:errcheck
	buf := new(bytes.Buffer)

	// Extract table id
	tableID, err := a.printTableInit(ctx, buf, db)
	if err != nil {
		return nil, err
	}

	// Generate table cells to be printed.
	err = a.printColumnsInfo(ctx, buf, db, tableID)
	if err != nil {
		return nil, err
	}

	// Print indexes.
	err = a.printIndexInfo(ctx, buf, db, tableID)
	if err != nil {
		return nil, err
	}

	err = a.printCheckConstraints(ctx, buf, db, tableID)
	if err != nil {
		return nil, err
	}

	err = a.printForeignKeyConstraints(ctx, buf, db, tableID)
	if err != nil {
		return nil, err
	}

	err = a.printReferencedBy(ctx, buf, db, tableID)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (a *postgresqlShowCreateTableAction) printTableInit(ctx context.Context, w io.Writer, db *sql.DB) (string, error) {
	var tableID, schema, relname string
	row := db.QueryRowContext(ctx, `SELECT /* pmm-agent */  c.oid,
	       n.nspname,
	       c.relname
	FROM pg_catalog.pg_class c
	         LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relname = $1
	  AND pg_catalog.pg_table_is_visible(c.oid)
	ORDER BY nspname, relname;`, a.params.Table)
	if err := row.Scan(&tableID, &schema, &relname); err != nil {
		if err == sql.ErrNoRows {
			return "", errors.Wrap(err, "Table not found")
		}
		return "", errors.WithStack(err)
	}
	fmt.Fprintf(w, "Table \"%s.%s\"\n", schema, relname) //nolint:errcheck
	return tableID, nil
}

func (a *postgresqlShowCreateTableAction) sealed() {}

func (a *postgresqlShowCreateTableAction) printColumnsInfo(ctx context.Context, w io.Writer, db *sql.DB, tableID string) error {
	rows, err := db.QueryContext(ctx, `SELECT /* pmm-agent */ a.attname,
       pg_catalog.format_type(a.atttypid, a.atttypmod),
       (SELECT substring(pg_catalog.pg_get_expr(d.adbin, d.adrelid) for 128)
        FROM pg_catalog.pg_attrdef d
        WHERE d.adrelid = a.attrelid
          AND d.adnum = a.attnum
          AND a.atthasdef),
       a.attnotnull,
       (SELECT c.collname
        FROM pg_catalog.pg_collation c,
             pg_catalog.pg_type t
        WHERE c.oid = a.attcollation
          AND t.oid = a.atttypid
          AND a.attcollation <> t.typcollation)                          AS attcollation,
       ''::pg_catalog.char                                               AS attidentity,
       a.attstorage,
       CASE WHEN a.attstattarget = -1 THEN NULL ELSE a.attstattarget END AS attstattarget,
       pg_catalog.col_description(a.attrelid, a.attnum)
FROM pg_catalog.pg_attribute a
WHERE a.attrelid = $1
  AND a.attnum > 0
  AND NOT a.attisdropped
ORDER BY a.attnum;`, tableID)
	if err != nil {
		return errors.WithStack(err)
	}
	defer rows.Close()

	tw := tabwriter.NewWriter(w, 0, 0, 1, ' ', tabwriter.Debug)

	fmt.Fprintln(tw, "Column\tType\tCollation\tNullable\tDefault\tStorage\tStats target\tDescription") //nolint:errcheck

	for rows.Next() {
		var ci columnInfo
		err = rows.Scan(
			&ci.Attname,
			&ci.FormatType,
			&ci.Substring,
			&ci.Attnotnull,
			&ci.Attcollation,
			&ci.Attidentity,
			&ci.Attstorage,
			&ci.Attstattarget,
			&ci.ColDescription,
		)
		if err != nil {
			return errors.WithStack(err)
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			ci.Attname,
			ci.FormatType,
			pointer.GetString(ci.Attcollation),
			formatNullable(ci.Attnotnull),
			pointer.GetString(ci.Substring),
			formatStorage(ci.Attstorage),
			pointer.GetString(ci.Attstattarget),
			pointer.GetString(ci.ColDescription),
		) //nolint:errcheck
	}
	err = rows.Err()
	if err != nil {
		return errors.WithStack(err)
	}
	return tw.Flush()
}

func (a *postgresqlShowCreateTableAction) printIndexInfo(ctx context.Context, w io.Writer, db *sql.DB, tableID string) error {
	rows, err := db.QueryContext(ctx, `SELECT /* pmm-agent */  c2.relname,
       i.indisprimary,
       i.indisunique,
       i.indisclustered,
       i.indisvalid,
       i.indrelid::regclass,
       pg_catalog.pg_get_indexdef(i.indexrelid, 0, false),
       pg_catalog.pg_get_constraintdef(con.oid, true),
       contype,
       condeferrable,
       condeferred,
       i.indisreplident,
       c2.reltablespace
FROM pg_catalog.pg_class c,
     pg_catalog.pg_class c2,
     pg_catalog.pg_index i
         LEFT JOIN pg_catalog.pg_constraint con
                   ON (conrelid = i.indrelid AND conindid = i.indexrelid AND contype IN ('p', 'u', 'x'))
WHERE c.oid = $1
  AND c.oid = i.indrelid
  AND i.indexrelid = c2.oid
ORDER BY i.indisprimary DESC, i.indisunique DESC, c2.relname`, tableID)
	if err != nil {
		return errors.WithStack(err)
	}
	defer rows.Close()

	var buf bytes.Buffer
	// We need it to be able to call Flush method to not write header if there are no rows.
	bw := bufio.NewWriter(&buf)

	fmt.Fprintln(bw, "Indexes:") //nolint:errcheck

	for rows.Next() {
		info := indexInfo{}
		err = rows.Scan(
			&info.Relname,
			&info.IsPrimary,
			&info.IsUnique,
			&info.IsClustered,
			&info.IsValid,
			&info.Indrelid,
			&info.PgGetIndexDef,
			&info.PgGetConstraintDef,
			&info.Contype,
			&info.Condeferrable,
			&info.Condeferred,
			&info.Indisreplident,
			&info.Reltablespace,
		)
		if err != nil {
			return errors.WithStack(err)
		}
		fmt.Fprintf(bw, "\t%q", info.Relname) //nolint:errcheck

		if pointer.GetString(info.Contype) == "x" {
			fmt.Fprintf(bw, " %s", pointer.GetString(info.PgGetConstraintDef)) //nolint:errcheck
		} else {
			// Label as primary key or unique (but not both).
			if info.IsPrimary {
				fmt.Fprintf(bw, " PRIMARY KEY,") //nolint:errcheck
			} else if info.IsUnique {
				if pointer.GetString(info.Contype) == "u" {
					fmt.Fprintf(bw, " UNIQUE CONSTRAINT,") //nolint:errcheck
				} else {
					fmt.Fprintf(bw, " UNIQUE,") //nolint:errcheck
				}
			}

			// Everything after "USING" is echoed verbatim.
			indexDef := pointer.GetString(info.PgGetIndexDef)
			usingPos := strings.Index(indexDef, " USING ")
			if usingPos != -1 {
				indexDef = indexDef[usingPos+7:]
			}
			fmt.Fprintf(bw, " %s", indexDef) //nolint:errcheck

			// Need these for deferrable PK/UNIQUE indexes.
			if pointer.GetBool(info.Condeferrable) {
				fmt.Fprintf(bw, " DEFERRABLE") //nolint:errcheck
			}

			if pointer.GetBool(info.Condeferred) {
				fmt.Fprintf(bw, " INITIALLY DEFERRED") //nolint:errcheck
			}
		}

		fmt.Fprintf(bw, "\n") //nolint:errcheck
		if err = bw.Flush(); err != nil {
			return errors.WithStack(err)
		}
	}
	err = rows.Err()
	if err != nil {
		return errors.WithStack(err)
	}
	w.Write(buf.Bytes()) //nolint:errcheck
	return nil
}

func (a *postgresqlShowCreateTableAction) printForeignKeyConstraints(ctx context.Context, w io.Writer, db *sql.DB, tableID string) error {
	rows, err := db.QueryContext(ctx, `SELECT /* pmm-agent */ conname,
       pg_catalog.pg_get_constraintdef(r.oid, true) as condef
FROM pg_catalog.pg_constraint r
WHERE r.conrelid = $1
  AND r.contype = 'f'
ORDER BY conname`, tableID)
	if err != nil {
		return errors.WithStack(err)
	}
	defer rows.Close()

	var buf bytes.Buffer
	// We need it to be able to call Flush method to not write header if there are no rows.
	bw := bufio.NewWriter(&buf)

	fmt.Fprintln(bw, "Foreign-key constraints:") //nolint:errcheck

	for rows.Next() {
		var conname, condef string
		err = rows.Scan(
			&conname,
			&condef,
		)
		if err != nil {
			return errors.WithStack(err)
		}
		fmt.Fprintf(bw, "\t%q %s\n", conname, condef) //nolint:errcheck

		if err = bw.Flush(); err != nil {
			return errors.WithStack(err)
		}
	}
	err = rows.Err()
	if err != nil {
		return errors.WithStack(err)
	}
	w.Write(buf.Bytes()) //nolint:errcheck
	return nil
}

func (a *postgresqlShowCreateTableAction) printReferencedBy(ctx context.Context, w io.Writer, db *sql.DB, tableID string) error {
	rows, err := db.QueryContext(ctx, `SELECT /* pmm-agent */ conname,
       conrelid::pg_catalog.regclass,
       pg_catalog.pg_get_constraintdef(c.oid, true) as condef
FROM pg_catalog.pg_constraint c
WHERE c.confrelid = $1
  AND c.contype = 'f'
ORDER BY conname`, tableID)
	if err != nil {
		return errors.WithStack(err)
	}
	defer rows.Close()

	var buf bytes.Buffer
	// We need it to be able to call Flush method to not write header if there are no rows.
	bw := bufio.NewWriter(&buf)

	fmt.Fprintln(bw, "Referenced by:") //nolint:errcheck

	for rows.Next() {
		var conname, conrelid, condef string
		err = rows.Scan(
			&conname,
			&conrelid,
			&condef,
		)
		if err != nil {
			return errors.WithStack(err)
		}
		fmt.Fprintf(bw, "\tTABLE %q CONSTRAINT %q %s\n", conrelid, conname, condef) //nolint:errcheck

		if err = bw.Flush(); err != nil {
			return errors.WithStack(err)
		}
	}
	err = rows.Err()
	if err != nil {
		return errors.WithStack(err)
	}
	w.Write(buf.Bytes()) //nolint:errcheck
	return nil
}

func (a *postgresqlShowCreateTableAction) printCheckConstraints(ctx context.Context, w io.Writer, db *sql.DB, tableID string) error {
	rows, err := db.QueryContext(ctx, `SELECT /* pmm-agent */ conname,
       pg_catalog.pg_get_constraintdef(r.oid, true) as condef
FROM pg_catalog.pg_constraint r
WHERE r.conrelid = $1
  AND r.contype = 'c'
ORDER BY conname`, tableID)
	if err != nil {
		return errors.WithStack(err)
	}
	defer rows.Close()

	var buf bytes.Buffer
	// We need it to be able to call Flush method to not write header if there are no rows.
	bw := bufio.NewWriter(&buf)

	fmt.Fprintln(bw, "Check constraints:") //nolint:errcheck

	for rows.Next() {
		var conname, condef string
		err = rows.Scan(
			&conname,
			&condef,
		)
		if err != nil {
			return errors.WithStack(err)
		}
		fmt.Fprintf(bw, "\t%q %s\n", conname, condef) //nolint:errcheck

		if err = bw.Flush(); err != nil {
			return errors.WithStack(err)
		}
	}
	err = rows.Err()
	if err != nil {
		return errors.WithStack(err)
	}
	w.Write(buf.Bytes()) //nolint:errcheck
	return nil
}

func formatNullable(attrnotNull bool) string {
	if attrnotNull {
		return "not null"
	}
	return ""
}

func formatStorage(attStorage string) string {
	switch attStorage {
	case "m":
		return "main"
	case "x":
		return "extended"
	case "p":
		return "plain"
	case "e":
		return "external"
	default:
		return ""
	}
}
