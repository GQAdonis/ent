// Copyright 2019-present Facebook Inc. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package schema

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/schema/field"
)

// Postgres is a postgres migration driver.
type Postgres struct {
	dialect.Driver
	schema  string
	version string
}

// init loads the Postgres version from the database for later use in the migration process.
// It returns an error if the server version is lower than v10.
func (d *Postgres) init(ctx context.Context, tx dialect.Tx) error {
	rows := &sql.Rows{}
	if err := tx.Query(ctx, "SHOW server_version_num", []interface{}{}, rows); err != nil {
		return fmt.Errorf("querying server version %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("server_version_num variable was not found")
	}
	var version string
	if err := rows.Scan(&version); err != nil {
		return fmt.Errorf("scanning version: %w", err)
	}
	if len(version) < 6 {
		return fmt.Errorf("malformed version: %s", version)
	}
	d.version = fmt.Sprintf("%s.%s.%s", version[:2], version[2:4], version[4:])
	if compareVersions(d.version, "10.0.0") == -1 {
		return fmt.Errorf("unsupported postgres version: %s", d.version)
	}
	return nil
}

// tableExist checks if a table exists in the database and current schema.
func (d *Postgres) tableExist(ctx context.Context, tx dialect.Tx, name string) (bool, error) {
	query, args := sql.Dialect(dialect.Postgres).
		Select(sql.Count("*")).From(sql.Table("tables").Schema("information_schema")).
		Where(sql.And(
			d.matchSchema(),
			sql.EQ("table_name", name),
		)).Query()
	return exist(ctx, tx, query, args...)
}

// tableExist checks if a foreign-key exists in the current schema.
func (d *Postgres) fkExist(ctx context.Context, tx dialect.Tx, name string) (bool, error) {
	query, args := sql.Dialect(dialect.Postgres).
		Select(sql.Count("*")).From(sql.Table("table_constraints").Schema("information_schema")).
		Where(sql.And(
			d.matchSchema(),
			sql.EQ("constraint_type", "FOREIGN KEY"),
			sql.EQ("constraint_name", name),
		)).Query()
	return exist(ctx, tx, query, args...)
}

// setRange sets restart the identity column to the given offset. Used by the universal-id option.
func (d *Postgres) setRange(ctx context.Context, tx dialect.Tx, t *Table, value int) error {
	if value == 0 {
		value = 1 // RESTART value cannot be < 1.
	}
	pk := "id"
	if len(t.PrimaryKey) == 1 {
		pk = t.PrimaryKey[0].Name
	}
	return tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s RESTART WITH %d", t.Name, pk, value), []interface{}{}, nil)
}

// table loads the current table description from the database.
func (d *Postgres) table(ctx context.Context, tx dialect.Tx, name string) (*Table, error) {
	rows := &sql.Rows{}
	query, args := sql.Dialect(dialect.Postgres).
		Select(
			"column_name", "data_type", "is_nullable", "column_default", "udt_name",
			"numeric_precision", "numeric_scale", "character_maximum_length",
		).
		From(sql.Table("columns").Schema("information_schema")).
		Where(sql.And(
			d.matchSchema(),
			sql.EQ("table_name", name),
		)).Query()
	if err := tx.Query(ctx, query, args, rows); err != nil {
		return nil, fmt.Errorf("postgres: reading table description %w", err)
	}
	// Call `Close` in cases of failures (`Close` is idempotent).
	defer rows.Close()
	t := NewTable(name)
	for rows.Next() {
		c := &Column{}
		if err := d.scanColumn(c, rows); err != nil {
			return nil, err
		}
		t.AddColumn(c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing rows %w", err)
	}
	idxs, err := d.indexes(ctx, tx, name)
	if err != nil {
		return nil, err
	}
	// Populate the index information to the table and its columns.
	// We do it manually, because PK and uniqueness information does
	// not exist when querying the information_schema.COLUMNS above.
	for _, idx := range idxs {
		switch {
		case idx.primary:
			for _, name := range idx.columns {
				c, ok := t.column(name)
				if !ok {
					return nil, fmt.Errorf("index %q column %q was not found in table %q", idx.Name, name, t.Name)
				}
				c.Key = PrimaryKey
				t.PrimaryKey = append(t.PrimaryKey, c)
			}
		case idx.Unique && len(idx.columns) == 1:
			name := idx.columns[0]
			c, ok := t.column(name)
			if !ok {
				return nil, fmt.Errorf("index %q column %q was not found in table %q", idx.Name, name, t.Name)
			}
			c.Key = UniqueKey
			c.Unique = true
			fallthrough
		default:
			t.addIndex(idx)
		}
	}
	return t, nil
}

// indexesQuery holds a query format for retrieving
// table indexes of the current schema.
const indexesQuery = `
SELECT i.relname AS index_name,
       a.attname AS column_name,
       idx.indisprimary AS primary,
       idx.indisunique AS unique,
       array_position(idx.indkey, a.attnum) as seq_in_index
FROM pg_class t,
     pg_class i,
     pg_index idx,
     pg_attribute a,
     pg_namespace n
WHERE t.oid = idx.indrelid
  AND i.oid = idx.indexrelid
  AND n.oid = t.relnamespace
  AND a.attrelid = t.oid
  AND a.attnum = ANY(idx.indkey)
  AND t.relkind = 'r'
  AND n.nspname = %s
  AND t.relname = '%s'
ORDER BY index_name, seq_in_index;
`

// indexesQuery returns the query (and its placeholders) for getting table indexes.
func (d *Postgres) indexesQuery(table string) (string, []interface{}) {
	if d.schema != "" {
		return fmt.Sprintf(indexesQuery, "$1", table), []interface{}{d.schema}
	}
	return fmt.Sprintf(indexesQuery, "CURRENT_SCHEMA()", table), nil
}

func (d *Postgres) indexes(ctx context.Context, tx dialect.Tx, table string) (Indexes, error) {
	rows := &sql.Rows{}
	query, args := d.indexesQuery(table)
	if err := tx.Query(ctx, query, args, rows); err != nil {
		return nil, fmt.Errorf("querying indexes for table %s: %w", table, err)
	}
	defer rows.Close()
	var (
		idxs  Indexes
		names = make(map[string]*Index)
	)
	for rows.Next() {
		var (
			seqindex        int
			name, column    string
			unique, primary bool
		)
		if err := rows.Scan(&name, &column, &primary, &unique, &seqindex); err != nil {
			return nil, fmt.Errorf("scanning index description: %w", err)
		}
		// If the index is prefixed with the table, it may was added by
		// `addIndex` and it should be trimmed. But, since entc prefixes
		// all indexes with schema-type, for uncountable types (like, media
		// or equipment) this isn't correct, and we fallback for the real-name.
		short := strings.TrimPrefix(name, table+"_")
		idx, ok := names[short]
		if !ok {
			idx = &Index{Name: short, Unique: unique, primary: primary, realname: name}
			idxs = append(idxs, idx)
			names[short] = idx
		}
		idx.columns = append(idx.columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return idxs, nil
}

// maxCharSize defines the maximum size of limited character types in Postgres (10 MB).
const maxCharSize = 10 << 20

// scanColumn scans the information a column from column description.
func (d *Postgres) scanColumn(c *Column, rows *sql.Rows) error {
	var (
		nullable            sql.NullString
		defaults            sql.NullString
		udt                 sql.NullString
		numericPrecision    sql.NullInt64
		numericScale        sql.NullInt64
		characterMaximumLen sql.NullInt64
	)
	if err := rows.Scan(&c.Name, &c.typ, &nullable, &defaults, &udt, &numericPrecision, &numericScale, &characterMaximumLen); err != nil {
		return fmt.Errorf("scanning column description: %w", err)
	}
	if nullable.Valid {
		c.Nullable = nullable.String == "YES"
	}
	switch c.typ {
	case "boolean":
		c.Type = field.TypeBool
	case "smallint":
		c.Type = field.TypeInt16
	case "integer":
		c.Type = field.TypeInt32
	case "bigint":
		c.Type = field.TypeInt64
	case "real":
		c.Type = field.TypeFloat32
	case "double precision":
		c.Type = field.TypeFloat64
	case "numeric", "decimal":
		c.Type = field.TypeFloat64
		// If precision is specified then we should take that into account.
		if numericPrecision.Valid {
			schemaType := fmt.Sprintf("%s(%d,%d)", c.typ, numericPrecision.Int64, numericScale.Int64)
			c.SchemaType = map[string]string{dialect.Postgres: schemaType}
		}
	case "text":
		c.Type = field.TypeString
		c.Size = maxCharSize + 1
	case "character", "character varying":
		c.Type = field.TypeString
		// If character maximum length is specified then we should take that into account.
		if characterMaximumLen.Valid {
			schemaType := fmt.Sprintf("varchar(%d)", characterMaximumLen.Int64)
			c.SchemaType = map[string]string{dialect.Postgres: schemaType}
		}
	case "date", "time", "timestamp", "timestamp with time zone", "timestamp without time zone":
		c.Type = field.TypeTime
	case "bytea":
		c.Type = field.TypeBytes
	case "jsonb":
		c.Type = field.TypeJSON
	case "uuid":
		c.Type = field.TypeUUID
	case "cidr", "inet", "macaddr", "macaddr8":
		c.Type = field.TypeOther
	case "ARRAY":
		c.Type = field.TypeOther
		if !udt.Valid {
			return fmt.Errorf("missing array type for column %q", c.Name)
		}
		// Note that for ARRAY types, the 'udt_name' column holds the array type
		// prefixed with '_'. For example, for 'integer[]' the result is '_int',
		// and for 'text[N][M]' the result is also '_text'. That's because, the
		// database ignores any size or multi-dimensions constraints.
		c.SchemaType = map[string]string{dialect.Postgres: "ARRAY"}
		c.typ = udt.String
	case "USER-DEFINED", "tstzrange", "interval":
		c.Type = field.TypeOther
		if !udt.Valid {
			return fmt.Errorf("missing user defined type for column %q", c.Name)
		}
		c.SchemaType = map[string]string{dialect.Postgres: udt.String}
	}
	switch {
	case !defaults.Valid || c.Type == field.TypeTime || callExpr(defaults.String):
		return nil
	case strings.Contains(defaults.String, "::"):
		parts := strings.Split(defaults.String, "::")
		defaults.String = strings.Trim(parts[0], "'")
		fallthrough
	default:
		return c.ScanDefault(defaults.String)
	}
}

// tBuilder returns the TableBuilder for the given table.
func (d *Postgres) tBuilder(t *Table) *sql.TableBuilder {
	b := sql.Dialect(dialect.Postgres).
		CreateTable(t.Name).IfNotExists()
	for _, c := range t.Columns {
		b.Column(d.addColumn(c))
	}
	for _, pk := range t.PrimaryKey {
		b.PrimaryKey(pk.Name)
	}
	if t.Annotation != nil {
		addChecks(b, t.Annotation)
	}
	return b
}

// cType returns the PostgreSQL string type for this column.
func (d *Postgres) cType(c *Column) (t string) {
	if c.SchemaType != nil && c.SchemaType[dialect.Postgres] != "" {
		return c.SchemaType[dialect.Postgres]
	}
	switch c.Type {
	case field.TypeBool:
		t = "boolean"
	case field.TypeUint8, field.TypeInt8, field.TypeInt16, field.TypeUint16:
		t = "smallint"
	case field.TypeInt32, field.TypeUint32:
		t = "int"
	case field.TypeInt, field.TypeUint, field.TypeInt64, field.TypeUint64:
		t = "bigint"
	case field.TypeFloat32:
		t = c.scanTypeOr("real")
	case field.TypeFloat64:
		t = c.scanTypeOr("double precision")
	case field.TypeBytes:
		t = "bytea"
	case field.TypeJSON:
		t = "jsonb"
	case field.TypeUUID:
		t = "uuid"
	case field.TypeString:
		t = "varchar"
		if c.Size > maxCharSize {
			t = "text"
		}
	case field.TypeTime:
		t = c.scanTypeOr("timestamp with time zone")
	case field.TypeEnum:
		// Currently, the support for enums is weak (application level only.
		// like SQLite). Dialect needs to create and maintain its enum type.
		t = "varchar"
	case field.TypeOther:
		t = c.typ
	default:
		panic(fmt.Sprintf("unsupported type %q for column %q", c.Type.String(), c.Name))
	}
	return t
}

// addColumn returns the ColumnBuilder for adding the given column to a table.
func (d *Postgres) addColumn(c *Column) *sql.ColumnBuilder {
	b := sql.Dialect(dialect.Postgres).
		Column(c.Name).Type(d.cType(c)).Attr(c.Attr)
	c.unique(b)
	if c.Increment {
		b.Attr("GENERATED BY DEFAULT AS IDENTITY")
	}
	c.nullable(b)
	d.writeDefault(b, c, "DEFAULT")
	if c.Collation != "" {
		b.Attr("COLLATE " + strconv.Quote(c.Collation))
	}
	return b
}

// writeDefault writes the `DEFAULT` clause to column builder
// if exists and supported by the driver.
func (d *Postgres) writeDefault(b *sql.ColumnBuilder, c *Column, clause string) {
	if c.Default == nil || !c.supportDefault() {
		return
	}
	attr := fmt.Sprint(c.Default)
	switch v := c.Default.(type) {
	case bool:
		attr = strconv.FormatBool(v)
	case string:
		if t := c.Type; t != field.TypeUUID && t != field.TypeTime && !t.Numeric() {
			// Escape single quote by replacing each with 2.
			attr = fmt.Sprintf("'%s'", strings.ReplaceAll(v, "'", "''"))
		}
	}
	b.Attr(clause + " " + attr)
}

// alterColumn returns list of ColumnBuilder for applying in order to alter a column.
func (d *Postgres) alterColumn(c *Column) (ops []*sql.ColumnBuilder) {
	b := sql.Dialect(dialect.Postgres)
	ops = append(ops, b.Column(c.Name).Type(d.cType(c)))
	if c.Nullable {
		ops = append(ops, b.Column(c.Name).Attr("DROP NOT NULL"))
	} else {
		ops = append(ops, b.Column(c.Name).Attr("SET NOT NULL"))
	}
	if c.Default != nil && c.supportDefault() {
		ops = append(ops, d.writeSetDefault(b.Column(c.Name), c))
	}
	return ops
}

func (d *Postgres) writeSetDefault(b *sql.ColumnBuilder, c *Column) *sql.ColumnBuilder {
	d.writeDefault(b, c, "SET DEFAULT")
	return b
}

// hasUniqueName reports if the index has a unique name in the schema.
func hasUniqueName(i *Index) bool {
	// Trim the "_key" suffix if it was added by Postgres for implicit indexes.
	name := strings.TrimSuffix(i.Name, "_key")
	suffix := strings.Join(i.columnNames(), "_")
	if !strings.HasSuffix(name, suffix) {
		return true // Assume it has a custom storage-key.
	}
	// The codegen prefixes by default indexes with the type name.
	// For example, an index "users"("name"), will named as "user_name".
	return name != suffix
}

// addIndex returns the query for adding an index to PostgreSQL.
func (d *Postgres) addIndex(i *Index, table string) *sql.IndexBuilder {
	name := i.Name
	if !hasUniqueName(i) {
		// Since index name should be unique in pg_class for schema,
		// we prefix it with the table name and remove on read.
		name = fmt.Sprintf("%s_%s", table, i.Name)
	}
	idx := sql.Dialect(dialect.Postgres).
		CreateIndex(name).IfNotExists().Table(table)
	if i.Unique {
		idx.Unique()
	}
	for _, c := range i.Columns {
		idx.Column(c.Name)
	}
	return idx
}

// dropIndex drops a Postgres index.
func (d *Postgres) dropIndex(ctx context.Context, tx dialect.Tx, idx *Index, table string) error {
	name := idx.Name
	build := sql.Dialect(dialect.Postgres)
	if prefix := table + "_"; !strings.HasPrefix(name, prefix) && !hasUniqueName(idx) {
		name = prefix + name
	}
	query, args := sql.Dialect(dialect.Postgres).
		Select(sql.Count("*")).From(sql.Table("table_constraints").Schema("information_schema")).
		Where(sql.And(
			d.matchSchema(),
			sql.EQ("constraint_type", "UNIQUE"),
			sql.EQ("constraint_name", name),
		)).
		Query()
	exists, err := exist(ctx, tx, query, args...)
	if err != nil {
		return err
	}
	query, args = build.DropIndex(name).Query()
	if exists {
		query, args = build.AlterTable(table).DropConstraint(name).Query()
	}
	return tx.Exec(ctx, query, args, nil)
}

// isImplicitIndex reports if the index was created implicitly for the unique column.
func (d *Postgres) isImplicitIndex(idx *Index, col *Column) bool {
	return strings.TrimSuffix(idx.Name, "_key") == col.Name && col.Unique
}

// renameColumn returns the statement for renaming a column.
func (d *Postgres) renameColumn(t *Table, old, new *Column) sql.Querier {
	return sql.Dialect(dialect.Postgres).
		AlterTable(t.Name).
		RenameColumn(old.Name, new.Name)
}

// renameIndex returns the statement for renaming an index.
func (d *Postgres) renameIndex(t *Table, old, new *Index) sql.Querier {
	if sfx := "_key"; strings.HasSuffix(old.Name, sfx) && !strings.HasSuffix(new.Name, sfx) {
		new.Name += sfx
	}
	if pfx := t.Name + "_"; strings.HasPrefix(old.realname, pfx) && !strings.HasPrefix(new.Name, pfx) {
		new.Name = pfx + new.Name
	}
	return sql.Dialect(dialect.Postgres).AlterIndex(old.realname).Rename(new.Name)
}

// matchSchema returns the predicate for matching table schema.
func (d *Postgres) matchSchema(columns ...string) *sql.Predicate {
	column := "table_schema"
	if len(columns) > 0 {
		column = columns[0]
	}
	if d.schema != "" {
		return sql.EQ(column, d.schema)
	}
	return sql.EQ(column, sql.Raw("CURRENT_SCHEMA()"))
}

// tables returns the query for getting the in the schema.
func (d *Postgres) tables() sql.Querier {
	return sql.Dialect(dialect.Postgres).
		Select("table_name").
		From(sql.Table("tables").Schema("information_schema")).
		Where(d.matchSchema())
}

// alterColumns returns the queries for applying the columns change-set.
func (d *Postgres) alterColumns(table string, add, modify, drop []*Column) sql.Queries {
	b := sql.Dialect(dialect.Postgres).AlterTable(table)
	for _, c := range add {
		b.AddColumn(d.addColumn(c))
	}
	for _, c := range modify {
		b.ModifyColumns(d.alterColumn(c)...)
	}
	for _, c := range drop {
		b.DropColumn(sql.Dialect(dialect.Postgres).Column(c.Name))
	}
	if len(b.Queries) == 0 {
		return nil
	}
	return sql.Queries{b}
}

// needsConversion reports if column "old" needs to be converted
// (by table altering) to column "new".
func (d *Postgres) needsConversion(old, new *Column) bool {
	oldT, newT := d.cType(old), d.cType(new)
	return oldT != newT && (oldT != "ARRAY" || !arrayType(newT))
}

// callExpr reports if the given string ~looks like a function call expression.
func callExpr(s string) bool {
	if parts := strings.Split(s, "::"); !strings.HasSuffix(s, ")") && strings.HasSuffix(parts[0], ")") {
		s = parts[0]
	}
	i, j := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if i == -1 || i > j || j != len(s)-1 {
		return false
	}
	for i, r := range s[:i] {
		if !isAlpha(r, i > 0) {
			return false
		}
	}
	return true
}

func isAlpha(r rune, digit bool) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || r == '_' || digit && '0' <= r && r <= '9'
}

// arrayType reports if the given string is an array type (e.g. int[], text[2]).
func arrayType(t string) bool {
	i, j := strings.LastIndexByte(t, '['), strings.LastIndexByte(t, ']')
	if i == -1 || j == -1 {
		return false
	}
	for _, r := range t[i+1 : j] {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// foreignKeys populates the tables foreign keys using the information_schema tables
func (d *Postgres) foreignKeys(ctx context.Context, tx dialect.Tx, tables []*Table) error {
	var tableLookup = make(map[string]*Table)
	for _, t := range tables {
		tableLookup[t.Name] = t
	}
	for _, t := range tables {
		rows := &sql.Rows{}
		query := fmt.Sprintf(fkQuery, t.Name)
		if err := tx.Query(ctx, query, []interface{}{}, rows); err != nil {
			return fmt.Errorf("querying foreign keys for table %s: %w", t.Name, err)
		}
		defer rows.Close()
		var tableFksLookup = make(map[string]*ForeignKey)
		for rows.Next() {
			var tableSchema, constraintName, tableName, columnName, refTableSchema, refTableName, refColumnName string
			if err := rows.Scan(&tableSchema, &constraintName, &tableName, &columnName, &refTableSchema, &refTableName, &refColumnName); err != nil {
				return fmt.Errorf("scanning index description: %w", err)
			}
			refTable := tableLookup[refTableName]
			if refTable == nil {
				return fmt.Errorf("could not find table: %s", refTableName)
			}
			column, ok := t.column(columnName)
			if !ok {
				return fmt.Errorf("could not find column: %s on table: %s", columnName, tableName)
			}
			refColumn, ok := refTable.column(refColumnName)
			if !ok {
				return fmt.Errorf("could not find ref column: %s on ref table: %s", refTableName, refColumnName)
			}
			if fk, ok := tableFksLookup[constraintName]; ok {
				if _, ok := fk.column(columnName); !ok {
					fk.Columns = append(fk.Columns, column)
				}
				if _, ok := fk.refColumn(refColumnName); !ok {
					fk.RefColumns = append(fk.RefColumns, refColumn)
				}
			} else {
				newFk := &ForeignKey{
					Symbol:     constraintName,
					Columns:    []*Column{column},
					RefTable:   refTable,
					RefColumns: []*Column{refColumn},
				}
				tableFksLookup[constraintName] = newFk
				t.AddForeignKey(newFk)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// fkQuery holds a query format for retrieving
// foreign keys of the current schema.
const fkQuery = `
SELECT tc.table_schema,
       tc.constraint_name,
       tc.table_name,
       kcu.column_name,
       ccu.table_schema AS foreign_table_schema,
       ccu.table_name   AS foreign_table_name,
       ccu.column_name  AS foreign_column_name
FROM information_schema.table_constraints AS tc
         JOIN information_schema.key_column_usage AS kcu
              ON tc.constraint_name = kcu.constraint_name
                  AND tc.table_schema = kcu.table_schema
         JOIN information_schema.constraint_column_usage AS ccu
              ON ccu.constraint_name = tc.constraint_name
                  AND ccu.table_schema = tc.table_schema
WHERE tc.constraint_type = 'FOREIGN KEY'
  AND tc.table_name = '%s'
order by constraint_name, kcu.ordinal_position;
`
