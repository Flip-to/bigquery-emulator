package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/goccy/googlesqlite"
	bigqueryv2 "google.golang.org/api/bigquery/v2"

	"github.com/goccy/bigquery-emulator/internal/connection"
	"github.com/goccy/bigquery-emulator/internal/metadata"
	"github.com/goccy/bigquery-emulator/internal/zsqltypes"
	"github.com/goccy/bigquery-emulator/types"
)

// syncSchemas creates the datasets requested by CREATE SCHEMA statements.
// The statement's OPTIONS (location, description, friendly_name) become the
// dataset's properties. A dataset that already exists is an error unless
// the statement said IF NOT EXISTS, as in BigQuery.
func syncSchemas(ctx context.Context, server *Server, defaultProjectID string, schemas []*googlesqlite.SchemaSpec) error {
	for _, spec := range schemas {
		projectID, datasetID := defaultProjectID, ""
		switch len(spec.NamePath) {
		case 1:
			datasetID = spec.NamePath[0]
		case 2:
			projectID, datasetID = spec.NamePath[0], spec.NamePath[1]
		default:
			return fmt.Errorf("unexpected schema name path: %v", spec.NamePath)
		}
		if err := createDatasetFromSchema(ctx, server, projectID, datasetID, spec); err != nil {
			return err
		}
	}
	return nil
}

func createDatasetFromSchema(ctx context.Context, server *Server, projectID, datasetID string, spec *googlesqlite.SchemaSpec) error {
	project, err := server.metaRepo.FindProject(ctx, projectID)
	if err != nil {
		return err
	}
	if project == nil {
		return errNotFound(fmt.Sprintf("project %s is not found", projectID))
	}
	if project.Dataset(datasetID) != nil {
		if spec.IfNotExists {
			return nil
		}
		return errDuplicate(fmt.Sprintf("Already Exists: Dataset %s:%s", projectID, datasetID))
	}
	content := &bigqueryv2.Dataset{
		Kind: "bigquery#dataset",
		Id:   fmt.Sprintf("%s:%s", projectID, datasetID),
		DatasetReference: &bigqueryv2.DatasetReference{
			ProjectId: projectID,
			DatasetId: datasetID,
		},
	}
	if v, ok := spec.Option("location"); ok {
		content.Location = v
	}
	if v, ok := spec.Option("description"); ok {
		content.Description = v
	}
	if v, ok := spec.Option("friendly_name"); ok {
		content.FriendlyName = v
	}
	conn, err := server.connMgr.Connection(ctx, projectID, datasetID)
	if err != nil {
		return fmt.Errorf("failed to get connection: %w", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer tx.RollbackIfNotCommitted()
	if err := project.AddDataset(
		ctx,
		tx.Tx(),
		metadata.NewDataset(server.metaRepo, projectID, datasetID, content, nil, nil, nil),
	); err != nil {
		if errors.Is(err, metadata.ErrDuplicatedDataset) && spec.IfNotExists {
			return nil
		}
		if errors.Is(err, metadata.ErrDuplicatedDataset) {
			return errDuplicate(err.Error())
		}
		return err
	}
	return tx.Commit()
}

// updateTableSchemas records the columns added by ALTER TABLE ADD COLUMN in
// the table metadata (the storage was altered by the driver).
func updateTableSchemas(ctx context.Context, server *Server, tables []*googlesqlite.TableSpec) error {
	for _, spec := range tables {
		if spec.IsTemp {
			continue
		}
		if len(spec.NamePath) != 3 {
			return fmt.Errorf("unexpected table name path: %v", spec.NamePath)
		}
		projectID, datasetID, tableID := spec.NamePath[0], spec.NamePath[1], spec.NamePath[2]
		project, err := server.metaRepo.FindProject(ctx, projectID)
		if err != nil {
			return err
		}
		dataset := project.Dataset(datasetID)
		if dataset == nil {
			return fmt.Errorf("dataset %s is not found", datasetID)
		}
		table := dataset.Table(tableID)
		if table == nil {
			// The table is not in the metadata (for example a table that
			// was created before the metadata sync existed): nothing to patch.
			continue
		}
		fields := make([]*bigqueryv2.TableFieldSchema, 0, len(spec.Columns))
		for _, column := range spec.Columns {
			fields = append(fields, types.TableFieldSchemaFromColumnType(column.Name, column.Type))
		}
		schema, err := toMap(&bigqueryv2.TableSchema{Fields: fields})
		if err != nil {
			return err
		}
		if err := withMetadataTx(ctx, server, projectID, datasetID, func(tx *connection.Tx) error {
			return table.Patch(ctx, tx.Tx(), map[string]interface{}{
				"schema":           schema,
				"lastModifiedTime": fmt.Sprintf("%d", time.Now().UnixMilli()),
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

// syncRoutines records CREATE / DROP of functions and table functions in the
// dataset's routine metadata, so routines.get and routines.list see routines
// created through SQL.
func syncRoutines(ctx context.Context, server *Server, cat *googlesqlite.ChangedCatalog) error {
	for _, fn := range cat.Function.Deleted {
		if fn.IsTemp {
			continue
		}
		if err := deleteRoutineMetadata(ctx, server, fn.NamePath); err != nil {
			return err
		}
	}
	for _, tvf := range cat.TableFunction.Deleted {
		if tvf.IsTemp {
			continue
		}
		if err := deleteRoutineMetadata(ctx, server, tvf.NamePath); err != nil {
			return err
		}
	}
	for _, fn := range cat.Function.Added {
		if fn.IsTemp {
			continue
		}
		routine := &bigqueryv2.Routine{
			RoutineType: routineTypeOfFunction(fn),
			Language:    routineLanguage(fn.Language),
		}
		for _, arg := range fn.Args {
			routine.Arguments = append(routine.Arguments, &bigqueryv2.Argument{
				Name:     arg.Name,
				DataType: standardSQLType(arg.Type),
			})
		}
		if fn.Return != nil {
			routine.ReturnType = standardSQLType(fn.Return)
		}
		routine.DefinitionBody = fn.Code
		routine.CreationTime = fn.CreatedAt.UnixMilli()
		routine.LastModifiedTime = fn.UpdatedAt.UnixMilli()
		if err := addRoutineMetadata(ctx, server, fn.NamePath, routine); err != nil {
			return err
		}
	}
	for _, tvf := range cat.TableFunction.Added {
		if tvf.IsTemp {
			continue
		}
		routine := &bigqueryv2.Routine{
			RoutineType:      "TABLE_VALUED_FUNCTION",
			Language:         "SQL",
			DefinitionBody:   tvf.Code,
			CreationTime:     tvf.CreatedAt.UnixMilli(),
			LastModifiedTime: tvf.UpdatedAt.UnixMilli(),
		}
		for _, arg := range tvf.Args {
			routine.Arguments = append(routine.Arguments, &bigqueryv2.Argument{
				Name:     arg.Name,
				DataType: standardSQLType(arg.Type),
			})
		}
		if err := addRoutineMetadata(ctx, server, tvf.NamePath, routine); err != nil {
			return err
		}
	}
	return nil
}

func routineTypeOfFunction(fn *googlesqlite.FunctionSpec) string {
	if fn.IsAggregate {
		return "AGGREGATE_FUNCTION"
	}
	return "SCALAR_FUNCTION"
}

func routineLanguage(language string) string {
	if strings.EqualFold(language, "js") || strings.EqualFold(language, "javascript") {
		return "JAVASCRIPT"
	}
	return "SQL"
}

// standardSQLType converts a driver type to a REST StandardSqlDataType.
func standardSQLType(t *googlesqlite.ColumnType) *bigqueryv2.StandardSqlDataType {
	if t == nil {
		return nil
	}
	switch zsqltypes.TypeKind(t.Kind) {
	case zsqltypes.ARRAY:
		return &bigqueryv2.StandardSqlDataType{
			TypeKind:         "ARRAY",
			ArrayElementType: standardSQLType(t.ElementType),
		}
	case zsqltypes.STRUCT:
		fields := make([]*bigqueryv2.StandardSqlField, 0, len(t.FieldTypes))
		for _, f := range t.FieldTypes {
			fields = append(fields, &bigqueryv2.StandardSqlField{Name: f.Name, Type: standardSQLType(f.Type)})
		}
		return &bigqueryv2.StandardSqlDataType{
			TypeKind:   "STRUCT",
			StructType: &bigqueryv2.StandardSqlStructType{Fields: fields},
		}
	}
	name := string(types.TypeFromKind(t.Kind))
	switch name {
	case "FLOAT":
		name = "FLOAT64"
	case "":
		name = "TYPE_KIND_UNSPECIFIED"
	}
	return &bigqueryv2.StandardSqlDataType{TypeKind: name}
}

func routineIDs(namePath []string) (projectID, datasetID, routineID string, err error) {
	if len(namePath) != 3 {
		return "", "", "", fmt.Errorf("unexpected routine name path: %v", namePath)
	}
	return namePath[0], namePath[1], namePath[2], nil
}

func addRoutineMetadata(ctx context.Context, server *Server, namePath []string, routine *bigqueryv2.Routine) error {
	projectID, datasetID, routineID, err := routineIDs(namePath)
	if err != nil {
		return err
	}
	return recordRoutine(ctx, server, projectID, datasetID, routineID, routine)
}

// recordRoutine stores the routine resource in the dataset metadata,
// replacing the record of a routine that is created again (CREATE OR REPLACE).
func recordRoutine(ctx context.Context, server *Server, projectID, datasetID, routineID string, routine *bigqueryv2.Routine) error {
	project, err := server.metaRepo.FindProject(ctx, projectID)
	if err != nil {
		return err
	}
	dataset := project.Dataset(datasetID)
	if dataset == nil {
		return fmt.Errorf("dataset %s is not found", datasetID)
	}
	routine.RoutineReference = &bigqueryv2.RoutineReference{
		ProjectId: projectID,
		DatasetId: datasetID,
		RoutineId: routineID,
	}
	if routine.CreationTime == 0 {
		routine.CreationTime = time.Now().UnixMilli()
	}
	if routine.LastModifiedTime == 0 {
		routine.LastModifiedTime = routine.CreationTime
	}
	content, err := toMap(routine)
	if err != nil {
		return err
	}
	return withMetadataTx(ctx, server, projectID, datasetID, func(tx *connection.Tx) error {
		if dataset.Routine(routineID) != nil {
			if err := dataset.DeleteRoutine(ctx, tx.Tx(), routineID); err != nil {
				return err
			}
		}
		return dataset.AddRoutine(
			ctx,
			tx.Tx(),
			metadata.NewRoutine(server.metaRepo, projectID, datasetID, routineID, content),
		)
	})
}

func deleteRoutineMetadata(ctx context.Context, server *Server, namePath []string) error {
	projectID, datasetID, routineID, err := routineIDs(namePath)
	if err != nil {
		return err
	}
	project, err := server.metaRepo.FindProject(ctx, projectID)
	if err != nil {
		return err
	}
	dataset := project.Dataset(datasetID)
	if dataset == nil || dataset.Routine(routineID) == nil {
		return nil
	}
	return withMetadataTx(ctx, server, projectID, datasetID, func(tx *connection.Tx) error {
		return dataset.DeleteRoutine(ctx, tx.Tx(), routineID)
	})
}

func withMetadataTx(ctx context.Context, server *Server, projectID, datasetID string, fn func(tx *connection.Tx) error) error {
	conn, err := server.connMgr.Connection(ctx, projectID, datasetID)
	if err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.RollbackIfNotCommitted()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func toMap(v interface{}) (map[string]interface{}, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, err
	}
	return out, nil
}
