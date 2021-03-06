package catalog

import (
	"errors"
	"fmt"

	"github.com/kyleconroy/sqlc/internal/sql/ast"
	sqlerr "github.com/kyleconroy/sqlc/internal/sql/errors"
)

func Build(stmts []ast.Statement) (*Catalog, error) {
	c := &Catalog{
		DefaultSchema: "main", // TODO: Needs to be public for PostgreSQL
		Schemas: []*Schema{
			&Schema{Name: "main"},
		},
	}
	for i := range stmts {
		if stmts[i].Raw == nil {
			continue
		}
		var err error
		switch n := stmts[i].Raw.Stmt.(type) {
		case *ast.AlterTableStmt:
			err = c.alterTable(n)
		case *ast.CommentOnColumnStmt:
			err = c.commentOnColumn(n)
		case *ast.CommentOnSchemaStmt:
			err = c.commentOnSchema(n)
		case *ast.CommentOnTableStmt:
			err = c.commentOnTable(n)
		case *ast.CommentOnTypeStmt:
			err = c.commentOnType(n)
		case *ast.CreateEnumStmt:
			err = c.createEnum(n)
		case *ast.CreateSchemaStmt:
			err = c.createSchema(n)
		case *ast.CreateTableStmt:
			err = c.createTable(n)
		case *ast.DropSchemaStmt:
			err = c.dropSchema(n)
		case *ast.DropTableStmt:
			err = c.dropTable(n)
		}
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}

func stringSlice(list *ast.List) []string {
	items := []string{}
	for _, item := range list.Items {
		if n, ok := item.(*ast.String); ok {
			items = append(items, n.Str)
		}
	}
	return items
}

func (c *Catalog) getSchema(name string) (*Schema, error) {
	for i := range c.Schemas {
		if c.Schemas[i].Name == name {
			return c.Schemas[i], nil
		}
	}
	return nil, sqlerr.SchemaNotFound(name)
}

func (c *Catalog) getTable(name *ast.TableName) (*Schema, *Table, error) {
	ns := name.Schema
	if ns == "" {
		ns = c.DefaultSchema
	}
	var s *Schema
	for i := range c.Schemas {
		if c.Schemas[i].Name == ns {
			s = c.Schemas[i]
			break
		}
	}
	if s == nil {
		return nil, nil, sqlerr.SchemaNotFound(ns)
	}
	t, _, err := s.getTable(name)
	if err != nil {
		return nil, nil, err
	}
	return s, t, nil
}

func (c *Catalog) getType(rel *ast.TypeName) (Type, error) {
	ns := rel.Schema
	if ns == "" {
		ns = c.DefaultSchema
	}
	s, err := c.getSchema(ns)
	if err != nil {
		return nil, err
	}
	return s.getType(rel)
}

func (c *Catalog) alterTable(stmt *ast.AlterTableStmt) error {
	var implemented bool
	for _, item := range stmt.Cmds.Items {
		switch cmd := item.(type) {
		case *ast.AlterTableCmd:
			switch cmd.Subtype {
			case ast.AT_AddColumn:
				implemented = true
			case ast.AT_AlterColumnType:
				implemented = true
			case ast.AT_DropColumn:
				implemented = true
			case ast.AT_DropNotNull:
				implemented = true
			case ast.AT_SetNotNull:
				implemented = true
			}
		}
	}
	if !implemented {
		return nil
	}
	_, table, err := c.getTable(stmt.Table)
	if err != nil {
		return err
	}

	for _, cmd := range stmt.Cmds.Items {
		switch cmd := cmd.(type) {
		case *ast.AlterTableCmd:
			idx := -1

			// Lookup column names for column-related commands
			switch cmd.Subtype {
			case ast.AT_AlterColumnType,
				ast.AT_DropColumn,
				ast.AT_DropNotNull,
				ast.AT_SetNotNull:
				for i, c := range table.Columns {
					if c.Name == *cmd.Name {
						idx = i
						break
					}
				}
				if idx < 0 && !cmd.MissingOk {
					return sqlerr.ColumnNotFound(table.Rel.Name, *cmd.Name)
				}
				// If a missing column is allowed, skip this command
				if idx < 0 && cmd.MissingOk {
					continue
				}
			}

			switch cmd.Subtype {

			case ast.AT_AddColumn:
				for _, c := range table.Columns {
					if c.Name == cmd.Def.Colname {
						return sqlerr.ColumnExists(table.Rel.Name, c.Name)
					}
				}
				table.Columns = append(table.Columns, &Column{
					Name:      cmd.Def.Colname,
					Type:      *cmd.Def.TypeName,
					IsNotNull: cmd.Def.IsNotNull,
				})

			case ast.AT_AlterColumnType:
				table.Columns[idx].Type = *cmd.Def.TypeName
				// table.Columns[idx].IsArray = isArray(d.TypeName)

			case ast.AT_DropColumn:
				table.Columns = append(table.Columns[:idx], table.Columns[idx+1:]...)

			case ast.AT_DropNotNull:
				table.Columns[idx].IsNotNull = false

			case ast.AT_SetNotNull:
				table.Columns[idx].IsNotNull = true

			}
		}
	}

	return nil
}

func (c *Catalog) createEnum(stmt *ast.CreateEnumStmt) error {
	ns := stmt.TypeName.Schema
	if ns == "" {
		ns = c.DefaultSchema
	}
	schema, err := c.getSchema(ns)
	if err != nil {
		return err
	}
	// Because tables have associated data types, the type name must also
	// be distinct from the name of any existing table in the same
	// schema.
	// https://www.postgresql.org/docs/current/sql-createtype.html
	tbl := &ast.TableName{
		Name: stmt.TypeName.Name,
	}
	if _, _, err := schema.getTable(tbl); err == nil {
		return sqlerr.RelationExists(tbl.Name)
	}
	if _, err := schema.getType(stmt.TypeName); err == nil {
		return sqlerr.TypeExists(tbl.Name)
	}
	schema.Types = append(schema.Types, &Enum{
		Name: stmt.TypeName.Name,
		Vals: stringSlice(stmt.Vals),
	})
	return nil
}

func (c *Catalog) createSchema(stmt *ast.CreateSchemaStmt) error {
	if stmt.Name == nil {
		return fmt.Errorf("create schema: empty name")
	}
	if _, err := c.getSchema(*stmt.Name); err == nil {
		if !stmt.IfNotExists {
			return sqlerr.SchemaExists(*stmt.Name)
		}
	}
	c.Schemas = append(c.Schemas, &Schema{Name: *stmt.Name})
	return nil
}

func (c *Catalog) createTable(stmt *ast.CreateTableStmt) error {
	ns := stmt.Name.Schema
	if ns == "" {
		ns = c.DefaultSchema
	}
	schema, err := c.getSchema(ns)
	if err != nil {
		return err
	}
	if _, _, err := schema.getTable(stmt.Name); err != nil {
		if !errors.Is(err, sqlerr.NotFound) {
			return err
		}
	} else if stmt.IfNotExists {
		return nil
	}
	tbl := Table{Rel: stmt.Name}
	for _, col := range stmt.Cols {
		tbl.Columns = append(tbl.Columns, &Column{
			Name:      col.Colname,
			Type:      *col.TypeName,
			IsNotNull: col.IsNotNull,
		})
	}
	schema.Tables = append(schema.Tables, &tbl)
	return nil
}

func (c *Catalog) dropSchema(stmt *ast.DropSchemaStmt) error {
	// TODO: n^2 in the worst-case
	for _, name := range stmt.Schemas {
		idx := -1
		for i := range c.Schemas {
			if c.Schemas[i].Name == name.Str {
				idx = i
			}
		}
		if idx == -1 {
			if stmt.MissingOk {
				continue
			}
			return sqlerr.SchemaNotFound(name.Str)
		}
		c.Schemas = append(c.Schemas[:idx], c.Schemas[idx+1:]...)
	}
	return nil
}

func (c *Catalog) dropTable(stmt *ast.DropTableStmt) error {
	for _, name := range stmt.Tables {
		ns := name.Schema
		if ns == "" {
			ns = c.DefaultSchema
		}
		schema, err := c.getSchema(ns)
		if errors.Is(err, sqlerr.NotFound) && stmt.IfExists {
			continue
		} else if err != nil {
			return err
		}

		_, idx, err := schema.getTable(name)
		if errors.Is(err, sqlerr.NotFound) && stmt.IfExists {
			continue
		} else if err != nil {
			return err
		}

		schema.Tables = append(schema.Tables[:idx], schema.Tables[idx+1:]...)
	}
	return nil
}

type Catalog struct {
	Name    string
	Schemas []*Schema
	Comment string

	DefaultSchema string
}

type Schema struct {
	Name    string
	Tables  []*Table
	Types   []Type
	Comment string
}

func (s *Schema) getType(rel *ast.TypeName) (Type, error) {
	for i := range s.Types {
		switch typ := s.Types[i].(type) {
		case *Enum:
			if typ.Name == rel.Name {
				return s.Types[i], nil
			}
		}
	}
	return nil, sqlerr.TypeNotFound(rel.Name)
}

func (s *Schema) getTable(rel *ast.TableName) (*Table, int, error) {
	for i := range s.Tables {
		if s.Tables[i].Rel.Name == rel.Name {
			return s.Tables[i], i, nil
		}
	}
	return nil, 0, sqlerr.RelationNotFound(rel.Name)
}

type Table struct {
	Rel     *ast.TableName
	Columns []*Column
	Comment string
}

// TODO: Should this just be ast Nodes?
type Column struct {
	Name      string
	Type      ast.TypeName
	IsNotNull bool
	Comment   string
}

type Type interface {
	isType()

	SetComment(string)
}

type Enum struct {
	Name    string
	Vals    []string
	Comment string
}

func (e *Enum) SetComment(c string) {
	e.Comment = c
}

func (e *Enum) isType() {
}
