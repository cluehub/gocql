// Copyright (c) 2015 The gocql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gocql

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// schema metadata for a keyspace
type KeyspaceMetadata struct {
	Name            string
	DurableWrites   bool
	StrategyClass   string
	StrategyOptions map[string]interface{}
	Tables          map[string]*TableMetadata
}

// schema metadata for a table (a.k.a. column family)
type TableMetadata struct {
	Keyspace          string
	Name              string
	KeyValidator      string
	Comparator        string
	DefaultValidator  string
	KeyAliases        []string
	ColumnAliases     []string
	ValueAlias        string
	PartitionKey      []*ColumnMetadata
	ClusteringColumns []*ColumnMetadata
	Columns           map[string]*ColumnMetadata
	OrderedColumns    []string
}

// schema metadata for a column
type ColumnMetadata struct {
	Keyspace       string
	Table          string
	Name           string
	ComponentIndex int
	Kind           string
	Validator      string
	Type           TypeInfo
	Order          ColumnOrder
	Index          ColumnIndexMetadata
}

// the ordering of the column with regard to its comparator
type ColumnOrder bool

const (
	ASC  ColumnOrder = false
	DESC             = true
)

type ColumnIndexMetadata struct {
	Name    string
	Type    string
	Options map[string]interface{}
}

// Column kind values
const (
	PARTITION_KEY  = "partition_key"
	CLUSTERING_KEY = "clustering_key"
	REGULAR        = "regular"
	COMPACT_VALUE  = "compact_value"
)

// default alias values
const (
	DEFAULT_KEY_ALIAS    = "key"
	DEFAULT_COLUMN_ALIAS = "column"
	DEFAULT_VALUE_ALIAS  = "value"
)

// queries the cluster for schema information for a specific keyspace
type schemaDescriber struct {
	session *Session
	mu      sync.Mutex

	cache map[string]*KeyspaceMetadata
}

// creates a session bound schema describer which will query and cache
// keyspace metadata
func newSchemaDescriber(session *Session) *schemaDescriber {
	return &schemaDescriber{
		session: session,
		cache:   map[string]*KeyspaceMetadata{},
	}
}

// returns the cached KeyspaceMetadata held by the describer for the named
// keyspace.
func (s *schemaDescriber) getSchema(keyspaceName string) (*KeyspaceMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// TODO handle schema change events

	metadata, found := s.cache[keyspaceName]
	if !found {
		// refresh the cache for this keyspace
		err := s.refreshSchema(keyspaceName)
		if err != nil {
			return nil, err
		}

		metadata = s.cache[keyspaceName]
	}

	return metadata, nil
}

// forcibly updates the current KeyspaceMetadata held by the schema describer
// for a given named keyspace.
func (s *schemaDescriber) refreshSchema(keyspaceName string) error {
	var err error

	// query the system keyspace for schema data
	// TODO retrieve concurrently
	keyspace, err := getKeyspaceMetadata(s.session, keyspaceName)
	if err != nil {
		return err
	}
	tables, err := getTableMetadata(s.session, keyspaceName)
	if err != nil {
		return err
	}
	columns, err := getColumnMetadata(s.session, keyspaceName)
	if err != nil {
		return err
	}

	// organize the schema data
	compileMetadata(s.session.cfg.ProtoVersion, keyspace, tables, columns)

	// update the cache
	s.cache[keyspaceName] = keyspace

	return nil
}

// "compiles" derived information about keyspace, table, and column metadata
// for a keyspace from the basic queried metadata objects returned by
// getKeyspaceMetadata, getTableMetadata, and getColumnMetadata respectively;
// Links the metadata objects together and derives the column composition of
// the partition key and clustering key for a table.
func compileMetadata(
	protoVersion int,
	keyspace *KeyspaceMetadata,
	tables []TableMetadata,
	columns []ColumnMetadata,
) {
	keyspace.Tables = make(map[string]*TableMetadata)
	for i := range tables {
		tables[i].Columns = make(map[string]*ColumnMetadata)

		keyspace.Tables[tables[i].Name] = &tables[i]
	}

	// add columns from the schema data
	for i := range columns {
		// decode the validator for TypeInfo and order
		validatorParsed := parseType(columns[i].Validator)
		columns[i].Type = validatorParsed.types[0]
		columns[i].Order = ASC
		if validatorParsed.reversed[0] {
			columns[i].Order = DESC
		}

		table := keyspace.Tables[columns[i].Table]
		table.Columns[columns[i].Name] = &columns[i]
		table.OrderedColumns = append(table.OrderedColumns, columns[i].Name)
	}

	if protoVersion == 1 {
		compileV1Metadata(tables)
	} else {
		compileV2Metadata(tables)
	}
}

// Compiles derived information from TableMetadata which have had
// ColumnMetadata added already. V1 protocol does not return as much
// column metadata as V2+ (because V1 doesn't support the "type" column in the
// system.schema_columns table) so determining PartitionKey and ClusterColumns
// is more complex.
func compileV1Metadata(tables []TableMetadata) {
	for i := range tables {
		table := &tables[i]

		// decode the key validator
		keyValidatorParsed := parseType(table.KeyValidator)
		// decode the comparator
		comparatorParsed := parseType(table.Comparator)

		// the partition key length is the same as the number of types in the
		// key validator
		table.PartitionKey = make([]*ColumnMetadata, len(keyValidatorParsed.types))

		// V1 protocol only returns "regular" columns from
		// system.schema_columns (there is no type field for columns)
		// so the alias information is used to
		// create the partition key and clustering columns

		// construct the partition key from the alias
		for i := range table.PartitionKey {
			var alias string
			if len(table.KeyAliases) > i {
				alias = table.KeyAliases[i]
			} else if i == 0 {
				alias = DEFAULT_KEY_ALIAS
			} else {
				alias = DEFAULT_KEY_ALIAS + strconv.Itoa(i+1)
			}

			column := &ColumnMetadata{
				Keyspace:       table.Keyspace,
				Table:          table.Name,
				Name:           alias,
				Type:           keyValidatorParsed.types[i],
				Kind:           PARTITION_KEY,
				ComponentIndex: i,
			}

			table.PartitionKey[i] = column
			table.Columns[alias] = column
		}

		// determine the number of clustering columns
		size := len(comparatorParsed.types)
		if comparatorParsed.isComposite {
			if len(comparatorParsed.collections) != 0 ||
				(len(table.ColumnAliases) == size-1 &&
					comparatorParsed.types[size-1].Type() == TypeVarchar) {
				size = size - 1
			}
		} else {
			if !(len(table.ColumnAliases) != 0 || len(table.Columns) == 0) {
				size = 0
			}
		}

		table.ClusteringColumns = make([]*ColumnMetadata, size)

		for i := range table.ClusteringColumns {
			var alias string
			if len(table.ColumnAliases) > i {
				alias = table.ColumnAliases[i]
			} else if i == 0 {
				alias = DEFAULT_COLUMN_ALIAS
			} else {
				alias = DEFAULT_COLUMN_ALIAS + strconv.Itoa(i+1)
			}

			order := ASC
			if comparatorParsed.reversed[i] {
				order = DESC
			}

			column := &ColumnMetadata{
				Keyspace:       table.Keyspace,
				Table:          table.Name,
				Name:           alias,
				Type:           comparatorParsed.types[i],
				Order:          order,
				Kind:           CLUSTERING_KEY,
				ComponentIndex: i,
			}

			table.ClusteringColumns[i] = column
			table.Columns[alias] = column
		}

		if size != len(comparatorParsed.types)-1 {
			alias := DEFAULT_VALUE_ALIAS
			if len(table.ValueAlias) > 0 {
				alias = table.ValueAlias
			}
			// decode the default validator
			defaultValidatorParsed := parseType(table.DefaultValidator)
			column := &ColumnMetadata{
				Keyspace: table.Keyspace,
				Table:    table.Name,
				Name:     alias,
				Type:     defaultValidatorParsed.types[0],
				Kind:     REGULAR,
			}
			table.Columns[alias] = column
		}
	}
}

// The simpler compile case for V2+ protocol
func compileV2Metadata(tables []TableMetadata) {
	for i := range tables {
		table := &tables[i]

		keyValidatorParsed := parseType(table.KeyValidator)
		table.PartitionKey = make([]*ColumnMetadata, len(keyValidatorParsed.types))

		clusteringColumnCount := componentColumnCountOfType(table.Columns, CLUSTERING_KEY)
		table.ClusteringColumns = make([]*ColumnMetadata, clusteringColumnCount)

		for _, columnName := range table.OrderedColumns {
			column := table.Columns[columnName]
			if column.Kind == PARTITION_KEY {
				table.PartitionKey[column.ComponentIndex] = column
			} else if column.Kind == CLUSTERING_KEY {
				table.ClusteringColumns[column.ComponentIndex] = column
			}
		}

	}
}

// returns the count of coluns with the given "kind" value.
func componentColumnCountOfType(columns map[string]*ColumnMetadata, kind string) int {
	maxComponentIndex := -1
	for _, column := range columns {
		if column.Kind == kind && column.ComponentIndex > maxComponentIndex {
			maxComponentIndex = column.ComponentIndex
		}
	}
	return maxComponentIndex + 1
}

// query only for the keyspace metadata for the specified keyspace from system.schema_keyspace
func getKeyspaceMetadata(
	session *Session,
	keyspaceName string,
) (*KeyspaceMetadata, error) {
	query := session.Query(
		`
		SELECT durable_writes, strategy_class, strategy_options
		FROM system.schema_keyspaces
		WHERE keyspace_name = ?
		`,
		keyspaceName,
	)
	// Set a routing key to avoid GetRoutingKey from computing the routing key
	// TODO use a separate connection (pool) for system keyspace queries.
	query.RoutingKey([]byte{})

	keyspace := &KeyspaceMetadata{Name: keyspaceName}
	var strategyOptionsJSON []byte

	err := query.Scan(
		&keyspace.DurableWrites,
		&keyspace.StrategyClass,
		&strategyOptionsJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("Error querying keyspace schema: %v", err)
	}

	err = json.Unmarshal(strategyOptionsJSON, &keyspace.StrategyOptions)
	if err != nil {
		return nil, fmt.Errorf(
			"Invalid JSON value '%s' as strategy_options for in keyspace '%s': %v",
			strategyOptionsJSON, keyspace.Name, err,
		)
	}

	return keyspace, nil
}

// query for only the table metadata in the specified keyspace from system.schema_columnfamilies
func getTableMetadata(session *Session, keyspaceName string) ([]TableMetadata, error) {

	var (
		scan func(iter *Iter, table *TableMetadata) bool
		stmt string

		keyAliasesJSON    []byte
		columnAliasesJSON []byte
	)

	if session.cfg.ProtoVersion < protoVersion4 {
		// we have key aliases
		// TODO: Do we need key_aliases?
		stmt = `
		SELECT
			columnfamily_name,
			key_validator,
			comparator,
			default_validator,
			key_aliases,
			column_aliases,
			value_alias
		FROM system.schema_columnfamilies
		WHERE keyspace_name = ?`

		scan = func(iter *Iter, table *TableMetadata) bool {
			return iter.Scan(
				&table.Name,
				&table.KeyValidator,
				&table.Comparator,
				&table.DefaultValidator,
				&keyAliasesJSON,
				&columnAliasesJSON,
				&table.ValueAlias,
			)
		}
	} else {
		stmt = `
		SELECT
			columnfamily_name,
			key_validator,
			comparator,
			default_validator
		FROM system.schema_columnfamilies
		WHERE keyspace_name = ?`

		scan = func(iter *Iter, table *TableMetadata) bool {
			return iter.Scan(
				&table.Name,
				&table.KeyValidator,
				&table.Comparator,
				&table.DefaultValidator,
			)
		}
	}

	// Set a routing key to avoid GetRoutingKey from computing the routing key
	// TODO use a separate connection (pool) for system keyspace queries.
	query := session.Query(stmt, keyspaceName)
	query.RoutingKey([]byte{})
	iter := query.Iter()

	tables := []TableMetadata{}
	table := TableMetadata{Keyspace: keyspaceName}

	for scan(iter, &table) {
		var err error

		// decode the key aliases
		if keyAliasesJSON != nil {
			table.KeyAliases = []string{}
			err = json.Unmarshal(keyAliasesJSON, &table.KeyAliases)
			if err != nil {
				iter.Close()
				return nil, fmt.Errorf(
					"Invalid JSON value '%s' as key_aliases for in table '%s': %v",
					keyAliasesJSON, table.Name, err,
				)
			}
		}

		// decode the column aliases
		if columnAliasesJSON != nil {
			table.ColumnAliases = []string{}
			err = json.Unmarshal(columnAliasesJSON, &table.ColumnAliases)
			if err != nil {
				iter.Close()
				return nil, fmt.Errorf(
					"Invalid JSON value '%s' as column_aliases for in table '%s': %v",
					columnAliasesJSON, table.Name, err,
				)
			}
		}

		tables = append(tables, table)
		table = TableMetadata{Keyspace: keyspaceName}
	}

	err := iter.Close()
	if err != nil && err != ErrNotFound {
		return nil, fmt.Errorf("Error querying table schema: %v", err)
	}

	return tables, nil
}

// query for only the column metadata in the specified keyspace from system.schema_columns
func getColumnMetadata(
	session *Session,
	keyspaceName string,
) ([]ColumnMetadata, error) {
	// Deal with differences in protocol versions
	var stmt string
	var scan func(*Iter, *ColumnMetadata, *[]byte) bool
	if session.cfg.ProtoVersion == 1 {
		// V1 does not support the type column, and all returned rows are
		// of kind "regular".
		stmt = `
			SELECT
				columnfamily_name,
				column_name,
				component_index,
				validator,
				index_name,
				index_type,
				index_options
			FROM system.schema_columns
			WHERE keyspace_name = ?
			`
		scan = func(
			iter *Iter,
			column *ColumnMetadata,
			indexOptionsJSON *[]byte,
		) bool {
			// all columns returned by V1 are regular
			column.Kind = REGULAR
			return iter.Scan(
				&column.Table,
				&column.Name,
				&column.ComponentIndex,
				&column.Validator,
				&column.Index.Name,
				&column.Index.Type,
				&indexOptionsJSON,
			)
		}
	} else {
		// V2+ supports the type column
		stmt = `
			SELECT
				columnfamily_name,
				column_name,
				component_index,
				validator,
				index_name,
				index_type,
				index_options,
				type
			FROM system.schema_columns
			WHERE keyspace_name = ?
			`
		scan = func(
			iter *Iter,
			column *ColumnMetadata,
			indexOptionsJSON *[]byte,
		) bool {
			return iter.Scan(
				&column.Table,
				&column.Name,
				&column.ComponentIndex,
				&column.Validator,
				&column.Index.Name,
				&column.Index.Type,
				&indexOptionsJSON,
				&column.Kind,
			)
		}
	}

	// get the columns metadata
	columns := []ColumnMetadata{}
	column := ColumnMetadata{Keyspace: keyspaceName}

	var indexOptionsJSON []byte

	query := session.Query(stmt, keyspaceName)
	// Set a routing key to avoid GetRoutingKey from computing the routing key
	// TODO use a separate connection (pool) for system keyspace queries.
	query.RoutingKey([]byte{})
	iter := query.Iter()

	for scan(iter, &column, &indexOptionsJSON) {
		var err error

		// decode the index options
		if indexOptionsJSON != nil {
			err = json.Unmarshal(indexOptionsJSON, &column.Index.Options)
			if err != nil {
				iter.Close()
				return nil, fmt.Errorf(
					"Invalid JSON value '%s' as index_options for column '%s' in table '%s': %v",
					indexOptionsJSON,
					column.Name,
					column.Table,
					err,
				)
			}
		}

		columns = append(columns, column)
		column = ColumnMetadata{Keyspace: keyspaceName}
	}

	err := iter.Close()
	if err != nil && err != ErrNotFound {
		return nil, fmt.Errorf("Error querying column schema: %v", err)
	}

	return columns, nil
}

// type definition parser state
type typeParser struct {
	input string
	index int
}

// the type definition parser result
type typeParserResult struct {
	isComposite bool
	types       []TypeInfo
	reversed    []bool
	collections map[string]TypeInfo
}

// Parse the type definition used for validator and comparator schema data
func parseType(def string) typeParserResult {
	parser := &typeParser{input: def}
	return parser.parse()
}

const (
	REVERSED_TYPE   = "org.apache.cassandra.db.marshal.ReversedType"
	COMPOSITE_TYPE  = "org.apache.cassandra.db.marshal.CompositeType"
	COLLECTION_TYPE = "org.apache.cassandra.db.marshal.ColumnToCollectionType"
	LIST_TYPE       = "org.apache.cassandra.db.marshal.ListType"
	SET_TYPE        = "org.apache.cassandra.db.marshal.SetType"
	MAP_TYPE        = "org.apache.cassandra.db.marshal.MapType"
)

// represents a class specification in the type def AST
type typeParserClassNode struct {
	name   string
	params []typeParserParamNode
	// this is the segment of the input string that defined this node
	input string
}

// represents a class parameter in the type def AST
type typeParserParamNode struct {
	name  *string
	class typeParserClassNode
}

func (t *typeParser) parse() typeParserResult {
	// parse the AST
	ast, ok := t.parseClassNode()
	if !ok {
		// treat this is a custom type
		return typeParserResult{
			isComposite: false,
			types: []TypeInfo{
				NativeType{
					typ:    TypeCustom,
					custom: t.input,
				},
			},
			reversed:    []bool{false},
			collections: nil,
		}
	}

	// interpret the AST
	if strings.HasPrefix(ast.name, COMPOSITE_TYPE) {
		count := len(ast.params)

		// look for a collections param
		last := ast.params[count-1]
		collections := map[string]TypeInfo{}
		if strings.HasPrefix(last.class.name, COLLECTION_TYPE) {
			count--

			for _, param := range last.class.params {
				// decode the name
				var name string
				decoded, err := hex.DecodeString(*param.name)
				if err != nil {
					Logger.Errorf(
						"Error parsing type '%s', contains collection name '%s' with an invalid format: %v",
						t.input,
						*param.name,
						err,
					)
					// just use the provided name
					name = *param.name
				} else {
					name = string(decoded)
				}
				collections[name] = param.class.asTypeInfo()
			}
		}

		types := make([]TypeInfo, count)
		reversed := make([]bool, count)

		for i, param := range ast.params[:count] {
			class := param.class
			reversed[i] = strings.HasPrefix(class.name, REVERSED_TYPE)
			if reversed[i] {
				class = class.params[0].class
			}
			types[i] = class.asTypeInfo()
		}

		return typeParserResult{
			isComposite: true,
			types:       types,
			reversed:    reversed,
			collections: collections,
		}
	} else {
		// not composite, so one type
		class := *ast
		reversed := strings.HasPrefix(class.name, REVERSED_TYPE)
		if reversed {
			class = class.params[0].class
		}
		typeInfo := class.asTypeInfo()

		return typeParserResult{
			isComposite: false,
			types:       []TypeInfo{typeInfo},
			reversed:    []bool{reversed},
		}
	}
}

func (class *typeParserClassNode) asTypeInfo() TypeInfo {
	if strings.HasPrefix(class.name, LIST_TYPE) {
		elem := class.params[0].class.asTypeInfo()
		return CollectionType{
			NativeType: NativeType{
				typ: TypeList,
			},
			Elem: elem,
		}
	}
	if strings.HasPrefix(class.name, SET_TYPE) {
		elem := class.params[0].class.asTypeInfo()
		return CollectionType{
			NativeType: NativeType{
				typ: TypeSet,
			},
			Elem: elem,
		}
	}
	if strings.HasPrefix(class.name, MAP_TYPE) {
		key := class.params[0].class.asTypeInfo()
		elem := class.params[1].class.asTypeInfo()
		return CollectionType{
			NativeType: NativeType{
				typ: TypeMap,
			},
			Key:  key,
			Elem: elem,
		}
	}

	// must be a simple type or custom type
	info := NativeType{typ: getApacheCassandraType(class.name)}
	if info.typ == TypeCustom {
		// add the entire class definition
		info.custom = class.input
	}
	return info
}

// CLASS := ID [ PARAMS ]
func (t *typeParser) parseClassNode() (node *typeParserClassNode, ok bool) {
	t.skipWhitespace()

	startIndex := t.index

	name, ok := t.nextIdentifier()
	if !ok {
		return nil, false
	}

	params, ok := t.parseParamNodes()
	if !ok {
		return nil, false
	}

	endIndex := t.index

	node = &typeParserClassNode{
		name:   name,
		params: params,
		input:  t.input[startIndex:endIndex],
	}
	return node, true
}

// PARAMS := "(" PARAM { "," PARAM } ")"
// PARAM := [ PARAM_NAME ":" ] CLASS
// PARAM_NAME := ID
func (t *typeParser) parseParamNodes() (params []typeParserParamNode, ok bool) {
	t.skipWhitespace()

	// the params are optional
	if t.index == len(t.input) || t.input[t.index] != '(' {
		return nil, true
	}

	params = []typeParserParamNode{}

	// consume the '('
	t.index++

	t.skipWhitespace()

	for t.input[t.index] != ')' {
		// look for a named param, but if no colon, then we want to backup
		backupIndex := t.index

		// name will be a hex encoded version of a utf-8 string
		name, ok := t.nextIdentifier()
		if !ok {
			return nil, false
		}
		hasName := true

		// TODO handle '=>' used for DynamicCompositeType

		t.skipWhitespace()

		if t.input[t.index] == ':' {
			// there is a name for this parameter

			// consume the ':'
			t.index++

			t.skipWhitespace()
		} else {
			// no name, backup
			hasName = false
			t.index = backupIndex
		}

		// parse the next full parameter
		classNode, ok := t.parseClassNode()
		if !ok {
			return nil, false
		}

		if hasName {
			params = append(
				params,
				typeParserParamNode{name: &name, class: *classNode},
			)
		} else {
			params = append(
				params,
				typeParserParamNode{class: *classNode},
			)
		}

		t.skipWhitespace()

		if t.input[t.index] == ',' {
			// consume the comma
			t.index++

			t.skipWhitespace()
		}
	}

	// consume the ')'
	t.index++

	return params, true
}

func (t *typeParser) skipWhitespace() {
	for t.index < len(t.input) && isWhitespaceChar(t.input[t.index]) {
		t.index++
	}
}

func isWhitespaceChar(c byte) bool {
	return c == ' ' || c == '\n' || c == '\t'
}

// ID := LETTER { LETTER }
// LETTER := "0"..."9" | "a"..."z" | "A"..."Z" | "-" | "+" | "." | "_" | "&"
func (t *typeParser) nextIdentifier() (id string, found bool) {
	startIndex := t.index
	for t.index < len(t.input) && isIdentifierChar(t.input[t.index]) {
		t.index++
	}
	if startIndex == t.index {
		return "", false
	}
	return t.input[startIndex:t.index], true
}

func isIdentifierChar(c byte) bool {
	return (c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		c == '-' ||
		c == '+' ||
		c == '.' ||
		c == '_' ||
		c == '&'
}
