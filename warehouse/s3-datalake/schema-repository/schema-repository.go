package schemarepository

import (
	"github.com/rudderlabs/rudder-server/utils/logger"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

func init() {
	pkgLogger = logger.NewLogger().Child("warehouse").Child("s3-datalake").Child("schema-repository")
}

var (
	pkgLogger    logger.LoggerI
	dataTypesMap = map[string]string{
		"boolean":  "boolean",
		"int":      "bigint",
		"bigint":   "bigint",
		"float":    "double",
		"string":   "varchar(512)",
		"text":     "varchar(max)",
		"datetime": "timestamp",
	}
	dataTypesMapToRudder = map[string]string{
		"boolean":      "boolean",
		"bigint":       "int",
		"double":       "float",
		"varchar(512)": "string",
		"varchar(max)": "text",
		"timestamp":    "datetime",
		"string":       "string",
	}
)

type SchemaRepository interface {
	FetchSchema(warehouse warehouseutils.WarehouseT) (warehouseutils.SchemaT, error)
	CreateSchema() (err error)
	CreateTable(tableName string, columnMap map[string]string) (err error)
	AddColumn(tableName string, columnName string, columnType string) (err error)
	AlterColumn(tableName string, columnName string, columnType string) (err error)
}

func hasGlueCatalogIdInConfig(config interface{}) bool {
	configMap := config.(map[string]interface{})
	if configMap["glueCatalogId"] == nil || configMap["glueCatalogId"].(string) == "" {
		return false
	}
	return true
}

func NewSchemaRepository(wh warehouseutils.WarehouseT, uploader warehouseutils.UploaderI) (SchemaRepository, error) {
	if hasGlueCatalogIdInConfig(wh.Destination.Config) {
		return NewGlueSchemaRepository(wh)
	}
	return NewLocalSchemaRepository(wh, uploader)
}
