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

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"math/bits"
	"reflect"
	"sort"
	"strconv"
	"time"

	"cloud.google.com/go/civil"
	sp "cloud.google.com/go/spanner"
	_ "github.com/lib/pq" // we will use database/sql package instead of using this package directly

	"github.com/cloudspannerecosystem/harbourbridge/internal"
	"github.com/cloudspannerecosystem/harbourbridge/profiles"
	"github.com/cloudspannerecosystem/harbourbridge/schema"
	"github.com/cloudspannerecosystem/harbourbridge/sources/common"
	"github.com/cloudspannerecosystem/harbourbridge/spanner/ddl"
	"github.com/cloudspannerecosystem/harbourbridge/streaming"
)

// InfoSchemaImpl postgres specific implementation for InfoSchema.
type InfoSchemaImpl struct {
	Db            *sql.DB
	SourceProfile profiles.SourceProfile
	TargetProfile profiles.TargetProfile
}

// StartChangeDataCapture is used for automatic triggering of Datastream job when
// performing a streaming migration.
func (isi InfoSchemaImpl) StartChangeDataCapture(ctx context.Context, conv *internal.Conv) (map[string]interface{}, error) {
	mp := make(map[string]interface{})
	streamingCfg, err := streaming.StartDatastream(ctx, isi.SourceProfile, isi.TargetProfile)
	if err != nil {
		err = fmt.Errorf("error starting datastream: %v", err)
		return nil, err
	}
	mp["streamingCfg"] = streamingCfg
	return mp, err
}

// StartStreamingMigration is used for automatic triggering of Dataflow job when
// performing a streaming migration.
func (isi InfoSchemaImpl) StartStreamingMigration(ctx context.Context, client *sp.Client, conv *internal.Conv, streamingInfo map[string]interface{}) error {
	streamingCfg, _ := streamingInfo["streamingCfg"].(streaming.StreamingCfg)

	err := streaming.StartDataflow(ctx, isi.TargetProfile, streamingCfg, conv)
	if err != nil {
		err = fmt.Errorf("error starting dataflow: %v", err)
		return err
	}
	return nil
}

// GetToDdl function below implement the common.InfoSchema interface.
func (isi InfoSchemaImpl) GetToDdl() common.ToDdl {
	return ToDdlImpl{}
}

// GetTableName returns table name.
func (isi InfoSchemaImpl) GetTableName(schema string, tableName string) string {
	if schema == "public" { // Drop 'public' prefix.
		return tableName
	}
	return fmt.Sprintf("%s.%s", schema, tableName)
}

// GetRowsFromTable returns a sql Rows object for a table.
func (isi InfoSchemaImpl) GetRowsFromTable(conv *internal.Conv, srcTable string) (interface{}, error) {
	// PostgreSQL schema and name can be arbitrary strings.
	// Ideally we would pass schema/name as a query parameter,
	// but PostgreSQL doesn't support this. So we quote it instead.
	q := fmt.Sprintf(`SELECT * FROM "%s"."%s";`, conv.SrcSchema[srcTable].Schema, srcTable)
	rows, err := isi.Db.Query(q)
	if err != nil {
		return nil, err
	}
	return rows, err
}

// ProcessDataRows performs data conversion for source database
// 'db'. For each table, we extract data using a "SELECT *" query,
// convert the data to Spanner data (based on the source and Spanner
// schemas), and write it to Spanner.  If we can't get/process data
// for a table, we skip that table and process the remaining tables.
//
// Note that the database/sql library has a somewhat complex model for
// returning data from rows.Scan. Scalar values can be returned using
// the native value used by the underlying driver (by passing
// *interface{} to rows.Scan), or they can be converted to specific go
// types. Array values are always returned as []byte, a string
// encoding of the array values. This string encoding is
// database/driver specific. For example, for PostgreSQL, array values
// are returned in the form "{v1,v2,..,vn}", where each v1,v2,...,vn
// is a PostgreSQL encoding of the respective array value.
//
// We choose to do all type conversions explicitly ourselves so that
// we can generate more targeted error messages: hence we pass
// *interface{} parameters to row.Scan.
func (isi InfoSchemaImpl) ProcessData(conv *internal.Conv, srcTable string, srcSchema schema.Table, spTable string, spCols []string, spSchema ddl.CreateTable) error {
	rowsInterface, err := isi.GetRowsFromTable(conv, srcTable)
	if err != nil {
		conv.Unexpected(fmt.Sprintf("Couldn't get data for table %s : err = %s", srcTable, err))
		return err
	}
	rows := rowsInterface.(*sql.Rows)
	defer rows.Close()
	srcCols, _ := rows.Columns()
	v, iv := buildVals(len(srcCols))
	for rows.Next() {
		err := rows.Scan(iv...)
		if err != nil {
			conv.Unexpected(fmt.Sprintf("Couldn't process sql data row: %s", err))
			// Scan failed, so we don't have any data to add to bad rows.
			conv.StatsAddBadRow(srcTable, conv.DataMode())
			continue
		}
		cvtCols, cvtVals, err := convertSQLRow(conv, srcTable, srcCols, srcSchema, spTable, spCols, spSchema, v)
		if err != nil {
			conv.Unexpected(fmt.Sprintf("Couldn't process sql data row: %s", err))
			conv.StatsAddBadRow(srcTable, conv.DataMode())
			conv.CollectBadRow(srcTable, srcCols, valsToStrings(v))
			continue
		}
		conv.WriteRow(srcTable, spTable, cvtCols, cvtVals)
	}
	return nil
}

// ConvertSQLRow performs data conversion for a single row of data
// returned from a 'SELECT *' query. ConvertSQLRow assumes that
// srcCols, spCols and srcVals all have the same length. Note that
// ConvertSQLRow returns cols as well as converted values. This is
// because cols can change when we add a column (synthetic primary
// key) or because we drop columns (handling of NULL values).
func convertSQLRow(conv *internal.Conv, srcTable string, srcCols []string, srcSchema schema.Table, spTable string, spCols []string, spSchema ddl.CreateTable, srcVals []interface{}) ([]string, []interface{}, error) {
	var vs []interface{}
	var cs []string
	for i := range srcCols {
		srcCd, ok1 := srcSchema.ColDefs[srcCols[i]]
		spCd, ok2 := spSchema.ColDefs[spCols[i]]
		if !ok1 || !ok2 {
			return nil, nil, fmt.Errorf("data conversion: can't find schema for column %s of table %s", srcCols[i], srcTable)
		}
		if srcVals[i] == nil {
			continue // Skip NULL values (nil is used by database/sql to represent NULL values).
		}
		var spVal interface{}
		var err error
		if spCd.T.IsArray {
			spVal, err = cvtSQLArray(conv, srcCd, spCd, srcVals[i])
		} else {
			spVal, err = cvtSQLScalar(conv, srcCd, spCd, srcVals[i])
		}
		if err != nil { // Skip entire row if we hit error.
			return nil, nil, fmt.Errorf("can't convert sql data for column %s of table %s: %w", srcCols[i], srcTable, err)
		}
		vs = append(vs, spVal)
		cs = append(cs, srcCols[i])
	}
	if aux, ok := conv.SyntheticPKeys[spTable]; ok {
		cs = append(cs, aux.Col)
		vs = append(vs, fmt.Sprintf("%d", int64(bits.Reverse64(uint64(aux.Sequence)))))
		aux.Sequence++
		conv.SyntheticPKeys[spTable] = aux
	}
	return cs, vs, nil
}

// GetRowCount with number of rows in each table.
func (isi InfoSchemaImpl) GetRowCount(table common.SchemaAndName) (int64, error) {
	// PostgreSQL schema and name can be arbitrary strings.
	// Ideally we would pass schema/name as a query parameter,
	// but PostgreSQL doesn't support this. So we quote it instead.
	q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s"."%s";`, table.Schema, table.Name)
	rows, err := isi.Db.Query(q)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var count int64
	if rows.Next() {
		err := rows.Scan(&count)
		return count, err
	}
	return 0, nil //Check if 0 is ok to return
}

// GetTables return list of tables in the selected database.
// TODO: All of the queries to get tables and table data should be in
// a single transaction to ensure we obtain a consistent snapshot of
// schema information and table data (pg_dump does something
// similar).
func (isi InfoSchemaImpl) GetTables() ([]common.SchemaAndName, error) {
	ignored := make(map[string]bool)
	// Ignore all system tables: we just want to convert user tables.
	for _, s := range []string{"information_schema", "postgres", "pg_catalog", "pg_temp_1", "pg_toast", "pg_toast_temp_1"} {
		ignored[s] = true
	}
	q := "SELECT table_schema, table_name FROM information_schema.tables where table_type = 'BASE TABLE'"
	rows, err := isi.Db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("couldn't get tables: %w", err)
	}
	defer rows.Close()
	var tableSchema, tableName string
	var tables []common.SchemaAndName
	for rows.Next() {
		rows.Scan(&tableSchema, &tableName)
		if !ignored[tableSchema] {
			tables = append(tables, common.SchemaAndName{Schema: tableSchema, Name: tableName})
		}
	}
	return tables, nil
}

// GetColumns returns a list of Column objects and names
func (isi InfoSchemaImpl) GetColumns(conv *internal.Conv, table common.SchemaAndName, constraints map[string][]string, primaryKeys []string) (map[string]schema.Column, []string, error) {
	q := `SELECT c.column_name, c.data_type, e.data_type, c.is_nullable, c.column_default, c.character_maximum_length, c.numeric_precision, c.numeric_scale
              FROM information_schema.COLUMNS c LEFT JOIN information_schema.element_types e
                 ON ((c.table_catalog, c.table_schema, c.table_name, 'TABLE', c.dtd_identifier)
                     = (e.object_catalog, e.object_schema, e.object_name, e.object_type, e.collection_type_identifier))
              where table_schema = $1 and table_name = $2 ORDER BY c.ordinal_position;`
	cols, err := isi.Db.Query(q, table.Schema, table.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't get schema for table %s.%s: %s", table.Schema, table.Name, err)
	}
	colDefs := make(map[string]schema.Column)
	var colNames []string
	var colName, dataType, isNullable string
	var colDefault, elementDataType sql.NullString
	var charMaxLen, numericPrecision, numericScale sql.NullInt64
	for cols.Next() {
		err := cols.Scan(&colName, &dataType, &elementDataType, &isNullable, &colDefault, &charMaxLen, &numericPrecision, &numericScale)
		if err != nil {
			conv.Unexpected(fmt.Sprintf("Can't scan: %v", err))
			continue
		}
		ignored := schema.Ignored{}
		for _, c := range constraints[colName] {
			// c can be UNIQUE, PRIMARY KEY, FOREIGN KEY,
			// or CHECK (based on msql, sql server, postgres docs).
			// We've already filtered out PRIMARY KEY.
			switch c {
			case "CHECK":
				ignored.Check = true
			case "FOREIGN KEY", "PRIMARY KEY", "UNIQUE":
				// Nothing to do here -- these are handled elsewhere.
			}
		}
		ignored.Default = colDefault.Valid
		c := schema.Column{
			Name:    colName,
			Type:    toType(dataType, elementDataType, charMaxLen, numericPrecision, numericScale),
			NotNull: common.ToNotNull(conv, isNullable),
			Ignored: ignored,
		}
		colDefs[colName] = c
		colNames = append(colNames, colName)
	}
	return colDefs, colNames, nil
}

// GetConstraints returns a list of primary keys and by-column map of
// other constraints.  Note: we need to preserve ordinal order of
// columns in primary key constraints.
// Note that foreign key constraints are handled in getForeignKeys.
func (isi InfoSchemaImpl) GetConstraints(conv *internal.Conv, table common.SchemaAndName) ([]string, map[string][]string, error) {
	q := `SELECT k.COLUMN_NAME, t.CONSTRAINT_TYPE
              FROM INFORMATION_SCHEMA.TABLE_CONSTRAINTS AS t
                INNER JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE AS k
                  ON t.CONSTRAINT_NAME = k.CONSTRAINT_NAME AND t.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA
              WHERE k.TABLE_SCHEMA = $1 AND k.TABLE_NAME = $2 ORDER BY k.ordinal_position;`
	rows, err := isi.Db.Query(q, table.Schema, table.Name)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var primaryKeys []string
	var col, constraint string
	m := make(map[string][]string)
	for rows.Next() {
		err := rows.Scan(&col, &constraint)
		if err != nil {
			conv.Unexpected(fmt.Sprintf("Can't scan: %v", err))
			continue
		}
		if col == "" || constraint == "" {
			conv.Unexpected(fmt.Sprintf("Got empty col or constraint"))
			continue
		}
		switch constraint {
		case "PRIMARY KEY":
			primaryKeys = append(primaryKeys, col)
		default:
			m[col] = append(m[col], constraint)
		}
	}
	return primaryKeys, m, nil
}

// GetForeignKeys returns a list of all the foreign key constraints.
func (isi InfoSchemaImpl) GetForeignKeys(conv *internal.Conv, table common.SchemaAndName) (foreignKeys []schema.ForeignKey, err error) {
	q := `SELECT 
		schema_name AS "TABLE_SCHEMA", 
		cl.relname AS "TABLE_NAME", 
		att2.attname AS "COLUMN_NAME", 
		att.attname AS "REF_COLUMN_NAME", 
		conname AS "CONSTRAINT_NAME"
		FROM (SELECT 
			UNNEST(con1.conkey) AS "parent", 
			UNNEST(con1.confkey) AS "child", 
			con1.confrelid, 
			con1.conrelid, 
			con1.conname, 
			ns.nspname AS schema_name
    		FROM PG_CLASS cl
        		JOIN PG_NAMESPACE ns ON cl.relnamespace = ns.oid
        		JOIN PG_CONSTRAINT con1 ON con1.conrelid = cl.oid
    			WHERE ns.nspname = $1 AND cl.relname = $2 AND con1.contype = 'f') con
   		JOIN PG_ATTRIBUTE att ON
       		att.attrelid = con.confrelid AND att.attnum = con.child
   		JOIN PG_CLASS cl ON
       		cl.oid = con.confrelid
   		JOIN PG_ATTRIBUTE att2 ON
       		att2.attrelid = con.conrelid AND att2.attnum = con.parent;`

	rows, err := isi.Db.Query(q, table.Schema, table.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refTable common.SchemaAndName
	var col, refCol, fKeyName string
	fKeys := make(map[string]common.FkConstraint)
	var keyNames []string
	for rows.Next() {
		err := rows.Scan(&refTable.Schema, &refTable.Name, &col, &refCol, &fKeyName)
		if err != nil {
			conv.Unexpected(fmt.Sprintf("Can't scan: %v", err))
			continue
		}
		tableName := isi.GetTableName(refTable.Schema, refTable.Name)
		if _, found := fKeys[fKeyName]; found {
			fk := fKeys[fKeyName]
			fk.Cols = append(fk.Cols, col)
			fk.Refcols = append(fk.Refcols, refCol)
			fKeys[fKeyName] = fk
			continue
		}
		fKeys[fKeyName] = common.FkConstraint{Name: fKeyName, Table: tableName, Refcols: []string{refCol}, Cols: []string{col}}
		keyNames = append(keyNames, fKeyName)
	}

	sort.Strings(keyNames)
	for _, k := range keyNames {
		foreignKeys = append(foreignKeys,
			schema.ForeignKey{
				Name:         fKeys[k].Name,
				Columns:      fKeys[k].Cols,
				ReferTable:   fKeys[k].Table,
				ReferColumns: fKeys[k].Refcols})
	}
	return foreignKeys, nil
}

// GetIndexes return a list of all indexes for the specified table.
// Note: Extracting index definitions from PostgreSQL information schema tables is complex.
// See https://stackoverflow.com/questions/6777456/list-all-index-names-column-names-and-its-table-name-of-a-postgresql-database/44460269#44460269
// for background.
func (isi InfoSchemaImpl) GetIndexes(conv *internal.Conv, table common.SchemaAndName) ([]schema.Index, error) {
	q := `SELECT
			irel.relname AS index_name,
			a.attname AS column_name,
			1 + Array_position(i.indkey, a.attnum) AS column_position,
			i.indisunique AS is_unique,
			CASE o.OPTION & 1 WHEN 1 THEN 'DESC' ELSE 'ASC' END AS order
		FROM pg_index AS i
		JOIN pg_class AS trel
		ON trel.oid = i.indrelid
		JOIN pg_namespace AS tnsp
		ON trel.relnamespace = tnsp.oid
		JOIN pg_class AS irel
		ON irel.oid = i.indexrelid
		CROSS JOIN LATERAL UNNEST (i.indkey) WITH ordinality AS c (colnum, ordinality)
		LEFT JOIN LATERAL UNNEST (i.indoption) WITH ordinality AS o (OPTION, ordinality)
		ON c.ordinality = o.ordinality
		JOIN pg_attribute AS a
		ON trel.oid = a.attrelid
			AND a.attnum = c.colnum
		WHERE tnsp.nspname= $1
			AND trel.relname= $2
			AND i.indisprimary = false
		GROUP BY tnsp.nspname,
           		trel.relname,
           		irel.relname,
           		a.attname,
           		array_position(i.indkey, a.attnum),
           		o.OPTION,i.indisunique
		ORDER BY irel.relname, array_position(i.indkey, a.attnum);`
	rows, err := isi.Db.Query(q, table.Schema, table.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var name, column, sequence, isUnique, collation string
	indexMap := make(map[string]schema.Index)
	var indexNames []string
	var indexes []schema.Index
	for rows.Next() {
		if err := rows.Scan(&name, &column, &sequence, &isUnique, &collation); err != nil {
			conv.Unexpected(fmt.Sprintf("Can't scan: %v", err))
			continue
		}
		if _, found := indexMap[name]; !found {
			indexNames = append(indexNames, name)
			indexMap[name] = schema.Index{Name: name, Unique: (isUnique == "true")}
		}
		index := indexMap[name]
		index.Keys = append(index.Keys, schema.Key{Column: column, Desc: (collation == "DESC")})
		indexMap[name] = index
	}
	for _, k := range indexNames {
		indexes = append(indexes, indexMap[k])
	}
	return indexes, nil
}

func toType(dataType string, elementDataType sql.NullString, charLen sql.NullInt64, numericPrecision, numericScale sql.NullInt64) schema.Type {
	switch {
	case dataType == "ARRAY" && elementDataType.Valid:
		return schema.Type{Name: elementDataType.String, ArrayBounds: []int64{-1}}
		// TODO: handle error cases.
		// TODO: handle case of multiple array bounds.
	case charLen.Valid:
		return schema.Type{Name: dataType, Mods: []int64{charLen.Int64}}
	case dataType == "numeric" && numericPrecision.Valid && numericScale.Valid && numericScale.Int64 != 0:
		return schema.Type{Name: dataType, Mods: []int64{numericPrecision.Int64, numericScale.Int64}}
	case dataType == "numeric" && numericPrecision.Valid:
		return schema.Type{Name: dataType, Mods: []int64{numericPrecision.Int64}}
	default:
		return schema.Type{Name: dataType}
	}
}

func cvtSQLArray(conv *internal.Conv, srcCd schema.Column, spCd ddl.ColumnDef, val interface{}) (interface{}, error) {
	a, ok := val.([]byte)
	if !ok {
		return nil, fmt.Errorf("can't convert array values to []byte")
	}
	return convArray(spCd.T, srcCd.Type.Name, conv.Location, string(a))
}

// cvtSQLScalar converts a values returned from a SQL query to a
// Spanner value.  In principle, we could just hand the values we get
// from the driver over to Spanner and have the Spanner client handle
// conversions and errors. However we handle the conversions
// explicitly ourselves so that we can generate more targeted error
// messages. Note that the caller is responsible for handling nil
// values (used to represent NULL). We handle each of the remaining
// cases of values returned by the database/sql library:
//
//	bool
//	[]byte
//	int64
//	float64
//	string
//	time.Time
func cvtSQLScalar(conv *internal.Conv, srcCd schema.Column, spCd ddl.ColumnDef, val interface{}) (interface{}, error) {
	switch spCd.T.Name {
	case ddl.Bool:
		switch v := val.(type) {
		case bool:
			return v, nil
		case string:
			return convBool(v)
		}
	case ddl.Bytes:
		switch v := val.(type) {
		case []byte:
			return v, nil
		}
	case ddl.Date:
		// The PostgreSQL driver uses time.Time to represent
		// dates.  Note that the database/sql library doesn't
		// document how dates are represented, so maybe this
		// isn't a driver issue, but a generic database/sql
		// issue.  We explicitly convert from time.Time to
		// civil.Date (used by the Spanner client library).
		switch v := val.(type) {
		case string:
			return convDate(v)
		case time.Time:
			return civil.DateOf(v), nil
		}
	case ddl.Int64:
		switch v := val.(type) {
		case []byte: // Parse as int64.
			return convInt64(string(v))
		case int64:
			return v, nil
		case float64: // Truncate.
			return int64(v), nil
		case string: // Parse as int64.
			return convInt64(v)
		}
	case ddl.Float64:
		switch v := val.(type) {
		case []byte: // Note: PostgreSQL uses []byte for numeric.
			return convFloat64(string(v))
		case int64:
			return float64(v), nil
		case float64:
			return v, nil
		case string:
			return convFloat64(v)
		}
	case ddl.Numeric:
		switch v := val.(type) {
		case []byte: // Note: PostgreSQL uses []byte for numeric.
			return convNumeric(conv, string(v))
		}
	case ddl.String:
		switch v := val.(type) {
		case bool:
			return strconv.FormatBool(v), nil
		case []byte:
			return string(v), nil
		case int64:
			return strconv.FormatInt(v, 10), nil
		case float64:
			return strconv.FormatFloat(v, 'g', -1, 64), nil
		case string:
			return v, nil
		case time.Time:
			return v.String(), nil
		}
	case ddl.Timestamp:
		switch v := val.(type) {
		case string:
			return convTimestamp(srcCd.Type.Name, conv.Location, v)
		case time.Time:
			return v, nil
		}
	case ddl.JSON:
		switch v := val.(type) {
		case string:
			return string(v), nil
		case []uint8:
			return string(v), nil
		}
	}
	return nil, fmt.Errorf("can't convert value of type %s to Spanner type %s", reflect.TypeOf(val), reflect.TypeOf(spCd.T))
}

// buildVals contructs interface{} value containers to scan row
// results into.  Returns both the underlying containers (as a slice)
// as well as an interface{} of pointers to containers to pass to
// rows.Scan.
func buildVals(n int) (v []interface{}, iv []interface{}) {
	v = make([]interface{}, n)
	for i := range v {
		iv = append(iv, &v[i])
	}
	return v, iv
}

func valsToStrings(vals []interface{}) []string {
	toString := func(val interface{}) string {
		if val == nil {
			return "NULL"
		}
		switch v := val.(type) {
		case *interface{}:
			val = *v
		}
		return fmt.Sprintf("%v", val)
	}
	var s []string
	for _, v := range vals {
		s = append(s, toString(v))
	}
	return s
}
