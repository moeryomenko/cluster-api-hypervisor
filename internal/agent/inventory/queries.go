package inventory

import _ "embed"

var (
	//go:embed queries/migrate.sql
	migrateQuery string

	//go:embed queries/record_operation.sql
	recordOperationQuery string

	//go:embed queries/get_operation_generation.sql
	operationGenerationQuery string

	//go:embed queries/upsert_vm.sql
	upsertVMQuery string

	//go:embed queries/get_vm.sql
	getVMQuery string

	//go:embed queries/begin_operation.sql
	beginOperationQuery string

	//go:embed queries/load_operation.sql
	loadOperationQuery string

	//go:embed queries/complete_operation.sql
	completeOperationQuery string

	//go:embed queries/pending_operations.sql
	pendingOperationsQuery string
)
