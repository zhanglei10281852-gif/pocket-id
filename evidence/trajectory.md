# 让中断的整库导入恢复到一致的数据代

> **Workspace:** F:\swe\098834\project-01

---

**User**

管理员从备份恢复实例时，如果导入在 actor 状态已经替换、数据库正在重建或 uploads 只写了一部分时被取消、失去独占租约、报错或进程退出，下一次启动不能把这种混合数据当作成功实例提供登录服务。请让每次导入拥有可持久识别的归档代和执行所有权，只有数据库、uploads 与嵌入式 Francis 状态全部提交到同一代后才报告成功；失败时要恢复原来的完整数据代，或者保留明确、可诊断且能继续或回滚的阶段，重启后的进程必须先收敛该阶段再正常启动。actor 恢复后数据库迁移失败、数据库完成后文件存储部分写入失败，以及租约丢失或进程在任意破坏性步骤间退出，需要按各自已发生的副作用采取恢复动作，旧执行随后返回不得继续提交。没有 actor 快照的旧归档仍应沿用保留现有 actor 状态的兼容语义，连接 standalone Francis 时仍不得悄悄半恢复携带 actor 数据的归档；无故障导入、SQLite 与 PostgreSQL 互导、filesystem、database 和 S3 存储的既有成功行为也要保持。

**Assistant**

我先来深入了解当前代码库中备份/恢复的实现。

toolName: view_folder

status: success

f:\swe\098834\project-01\backend

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_service.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\actors_backup.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\export_service.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\storage.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\db_bootstrap.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\bootstrap.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\database.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\actors_bootstrap.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\model\kv.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_service_test.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\instanceid\instanceid.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\utils\db_migration_util.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\s3.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\actors_backup_test.go

让我查看 Francis 库的 Backup/Restore 实现语义，以及 exclusive lease 的机制。

toolName: Shell

status: success

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\go.mod

toolName: file_search

status: success

toolName: file_search

status: success

toolName: file_search

status: success

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: C:\Users\czxy4\go\pkg\mod\github.com\italypaleale\francis@v0.1.1\components\sqlite\sqlite-backup.go

toolName: view_files

status: success

filePath: C:\Users\czxy4\go\pkg\mod\github.com\italypaleale\francis@v0.1.1\clusteradmin\clusteradmin.go

现在让我查看环境配置、Docker 部署结构以及 main 启动流程，理解进程启动入口。

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\common\env_config.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\resources\migrations\sqlite\20250630000000_kv_table.up.sql

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\resources\migrations\sqlite\20251110000000_storage_table.up.sql

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\cmd\main.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\root.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\resources\migrations\postgres\20251110000000_storage_table.up.sql

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\export.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\utils\db_util.go

toolName: search_by_regex

status: success

toolName: view_folder

status: success

f:\swe\098834\project-01\tests\setup

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\tests\setup\docker-compose.yml

让我再查看几个关键文件：schema 奇偶测试、文件清理任务、以及最新迁移的结构。

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\utils\db_schema_parity_test.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\job\file_cleanup_job.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\app_images_bootstrap.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\export_service_test.go

让我检查迁移中的视图、触发器、序列使用情况，这对 schema 换名方案很关键。

toolName: search_by_regex

status: success

toolName: search_by_regex

status: failed

toolName: search_by_regex

status: success

让我确认 Postgres 的 Francis Restore 原子性、S3 测试方式，以及现有迁移中的布尔/整数类型约定。

toolName: view_files

status: success

filePath: C:\Users\czxy4\go\pkg\mod\github.com\italypaleale\francis@v0.1.1\components\postgres\postgres-backup.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\s3_test.go

toolName: search_by_regex

status: success

toolName: file_search

status: success

toolName: file_search

status: success

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\resources\migrations\sqlite\20261006120000_sqlite_postgres_schema_alignment.up.sql

我已经充分理解了架构。现在建立任务清单并开始实现。

toolName: todo_write

status: success

Todos updated: 9 items

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\resources\migrations\postgres\20251110000000_storage_table.down.sql

开始实现。先创建迁移文件（SQLite 和 PostgreSQL 各一对）：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\resources\migrations\sqlite\20261103120000_import_journal.up.sql

changes: undefined

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\resources\migrations\sqlite\20261103120000_import_journal.down.sql

changes: undefined

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\resources\migrations\postgres\20261103120000_import_journal.up.sql

changes: undefined

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\resources\migrations\postgres\20261103120000_import_journal.down.sql

changes: undefined

toolName: todo_write

status: success

Todos updated: 9 items

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_journal.go

changes: undefined

修正 `sqlNoRows` 引用，直接使用 `database/sql.ErrNoRows`：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_journal.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_journal.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_journal.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_journal.go

changes: undefined

现在实现 schema 影子换名/恢复机制：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

现在实现 uploads 分代切换（storage 层）。先定义接口与 database 后端实现：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\upload_generation.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\upload_generation.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\upload_generation.go

changes: undefined

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem_upload_generation.go

changes: undefined

现在实现 S3 分代切换：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\s3_upload_generation.go

changes: undefined

在 S3 的 List/Walk 中隐藏导入保留前缀：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\s3.go

changes: undefined

现在重写导入编排核心。先更新 export 排除列表和 drop 排除规则：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\export_service.go

changes: undefined

现在重写 `import_service.go`（保留解析/插入/迁移逻辑，更新 drop 排除规则，移除旧编排）：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_service.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_service.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_service.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

现在给 filesystem storage 暴露根路径，并添加工作目录解析助手：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\upload_generation.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\upload_generation.go

changes: undefined

toolName: search_by_regex

status: success

toolName: file_search

status: success

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\s3_upload_generation.go

changes: undefined

现在编写核心编排文件：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

修正编排文件中的几个问题：carrier 复用、resume 重建逻辑、所有权丢失标记的竞态：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: Shell

status: success

现在将独占租约获取逻辑移到 bootstrap 包，供 CLI 和启动恢复共用：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\exclusive_access.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\exclusive_access.go

changes: undefined

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\import_recovery.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\import_recovery.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\import_recovery.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\import_recovery.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\import_recovery.go

changes: undefined

将恢复门接入 Bootstrap 启动流程：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\bootstrap.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\bootstrap.go

changes: undefined

现在重写 CLI import 命令：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

changes: undefined

toolName: search_by_regex

status: success

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\import_recovery.go

changes: undefined

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

changes: undefined

toolName: Shell

status: success

编译通过。现在重构 actor 恢复辅助函数并更新原有测试：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\actors_backup_test.go

changes: undefined

现在加入测试用故障注入钩子，并修正 rollback 中阶段判断（j.Phase 需随推进更新）：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_service.go

changes: undefined

现在在编排中使用统一的 `advance` 包装并插入故障钩子：

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: todo_write

status: success

Todos updated: 9 items

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: Shell

status: success

现有测试通过。查看 service 包的测试初始化方式，然后编写新测试：

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\export_service_test.go

现在编写测试。先是日志与所有权 fencing 测试：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_journal_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_journal_test.go

changes: undefined

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema_test.go

changes: undefined

扩展 actors stub 以记录多次 Restore，然后编写编排集成测试：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\actors_backup_test.go

changes: undefined

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration_test.go

changes: undefined

重写测试文件，修正类型与辅助引用：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration_test.go

changes: undefined

toolName: Shell

status: success

`ALTER INDEX RENAME` 报错，让我确认驱动实际支持的 SQLite 语法：

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\zz_tmp_ver_test.go

changes: undefined

toolName: Shell

status: success

SQLite 根本不支持 `ALTER INDEX RENAME`。改用"表先换名→按定义重建索引"的方案：

toolName: delete_file

status: success

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

toolName: Shell

status: success

LIKE 中 `_` 是通配符可能造成误匹配。先加调试确认影子表状态：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\zz_tmp_debug_test.go

changes: undefined

toolName: Shell

status: success

toolName: Shell

status: success

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration_test.go

测试辅助函数 `kvValue` 在无行时误报 true（Gorm Scan 到基础类型无错误）。修复：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration_test.go

changes: undefined

修复影子前缀匹配（SQLite 用 GLOB 避免 `_` 通配；Postgres 转义下划线）：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema_test.go

changes: undefined

toolName: delete_file

status: success

toolName: Shell

status: success

剩一个失败：uploads_prepared 崩溃后恢复，旧 kv 行未回来。加调试观察恢复后的表状态：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\zz_tmp_debug2_test.go

changes: undefined

toolName: Shell

status: success

找到 bug：rollback 在 switch 开始前（uploads_prepared）会误删从未被切换的 live 文件。按"暂存对象是否还存在"精确判定是否已切换，修复 fs 和 S3：

toolName: edit_file_search_replace

status: failed

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem_upload_generation.go

changes: undefined

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem_upload_generation.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem_upload_generation.go

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem_upload_generation.go

changes: undefined

同样修复 S3 Rollback：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\s3_upload_generation.go

changes: undefined

toolName: delete_file

status: success

toolName: Shell

status: success

核心测试通过。现在为 storage 层的分代切换补充单元测试：

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem_upload_generation_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem_upload_generation_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem_upload_generation_test.go

changes: undefined

toolName: Write

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\s3_upload_generation_test.go

changes: undefined

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\s3_upload_generation_test.go

changes: undefined

toolName: Shell

status: success

运行整个后端测试套件：

toolName: todo_write

status: success

Todos updated: 9 items

toolName: Shell

status: success

全部测试通过。运行 vet 与 golangci-lint：

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

确认这 3 个 goimports 警告是基线既有的工具链漂移问题，非本次引入。确认改动已恢复：

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

让我审查 cmds/francis_runtime.go 确认 standalone 检查逻辑仍然完整：

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\francis_runtime.go

调整 CLI 顺序：standalone 的归档拒绝检查提前到存储初始化/租约获取之前：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

changes: undefined

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

去掉重复初始化，改为基于配置计算 workDir，使 standalone 拒绝发生在存储/租约初始化之前：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\upload_generation.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\storage\filesystem_upload_generation.go

changes: undefined

toolName: Shell

status: success

复查所有 `NewDatabase` 调用点和恢复逻辑的潜在交互：

toolName: search_by_regex

status: success

运行全量测试验证重排：

toolName: Shell

status: success

尝试运行 PostgreSQL 奇偶测试（若本地有测试库）：

toolName: Shell

status: success

给回滚后的外键完整性增加显式校验（`PRAGMA foreign_key_check`）：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration_test.go

changes: undefined

增加"committing 阶段崩溃→前向收敛完成提交"的测试：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration_test.go

changes: undefined

toolName: Shell

status: success

完整复审编排文件：

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

changes: undefined

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\import_recovery.go

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

修正一个边界问题：阶段副作用已提交但日志更新失败时，回滚应按已提交副作用处理。让推进先乐观更新内存阶段：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

changes: undefined

toolName: Shell

status: success

增加"崩溃后用同一归档 --resume 成功完成"的测试：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration_test.go

changes: undefined

toolName: Shell

status: success

全部通过。运行 gofmt 检查所有新文件：

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

全部是 CRLF 行尾导致（仓库 autocrlf 工作树统一现象）。验证转换 LF 后无真实格式问题：

toolName: Shell

status: success

修复真实格式问题（保留工作树 CRLF 约定）：

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

最终全量验证：

toolName: Shell

status: success

toolName: Shell

status: success

检查是否遗留临时文件并确认最终变更清单：

toolName: Shell

status: success

完整浏览编排文件剩余部分，做最终确认：

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_orchestration.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

changes: undefined

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\cmds\import.go

changes: undefined

toolName: Shell

status: success

最终全量校验：

toolName: Shell

status: success

toolName: Shell

status: success

修复测试中的真实 lint 问题（`rows.Err` 未检查）：

toolName: edit_file_search_replace

status: success

filePath: f:\swe\098834\project-01\backend\internal\service\import_schema_test.go

changes: undefined

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

所有剩余的 goimports 提示均为本地 CRLF 行尾环境问题（全仓库每个文件都一样，CI Linux 上不存在），我的代码无任何语义 lint 问题。最后确认 bootstrap 改动段落：

toolName: view_files

status: success

filePath: f:\swe\098834\project-01\backend\internal\bootstrap\bootstrap.go

toolName: todo_write

status: success

Todos updated: 9 items

已完成导入事务化改造。所有后端单元测试（`-tags=exclude_frontend,unit`）、`go vet`、编译全部通过。

## 实现总览

### 核心机制：持久的「归档代 + 执行所有权」

**新表 `import_journal`**（[sqlite](backend/resources/migrations/sqlite/20261103120000_import_journal.up.sql) / [postgres](backend/resources/migrations/postgres/20261103120000_import_journal.up.sql) 各一对迁移，类型满足跨库导出奇偶）：
- `generation_id`（归档代）、`token`（影子对象短标识）、`owner_id` + `owner_deadline`（执行所有权/fencing）
- `phase`（prepared → actors_restored → schema_shadowed → database_restored → uploads_prepared → uploads_committed → committing）、`old_actors`（旧 actor 代备份）、`last_error`（可诊断）
- 该表在 schema 重建时被保留、从导出中排除；导入命令未迁移时用 `CREATE IF NOT EXISTS` 自建。

### 三个组件的同代原子化提交

1. **数据库**（[import_schema.go](backend/internal/service/import_schema.go)）：导入前把全部旧表（含索引、`schema_migrations`）在**同一事务**里换名成 `i<token>_*` 影子集，再迁移+装载新 schema；提交时才删除影子。SQLite 用「换表后按定义重建索引」（SQLite 无 `ALTER INDEX RENAME`），Postgres 用事务化 `RENAME`。
2. **uploads**（[storage/upload_generation.go](backend/internal/storage/upload_generation.go) 等）：
   - filesystem：在 upload 根的**同级隐藏目录**暂存，逐文件原子 rename 切换（旧文件先移入 `old/`），对 Walk/导出完全不可见；
   - database：随 schema 影子整体换表，并把归档中没有的旧文件 `NOT EXISTS` 前带（保持既有合并语义）；
   - S3：`.import-staging/` 暂存前缀 + manifest + CopyObject 切换 + `.import-backup/` 回滚，List/Walk 隐藏保留前缀。
3. **嵌入式 Francis**：先 `Backup` 旧代到 journal，再 `Restore` 新代（provider 自身原子）。

### 失败/中断恢复

- 编排（[import_orchestration.go](backend/internal/service/import_orchestration.go)）：任一阶段失败按**已发生的副作用**回滚（uploads→DB→actors），journal 删除才算终态；租约丢失/所有权被抢则**旧执行立即停手不提交**（fencing，每个阶段推进都 `WHERE owner_id=?`）。
- 进程被杀：journal 留存，服务器启动时 [RecoverInterruptedImports](backend/internal/bootstrap/import_recovery.go) 在**迁移、storage、actor host、路由之前**先收敛——committing 前向完成、其余回滚到完整旧代；嵌入式经独占租约串行化（并把租约助手移到 [bootstrap/exclusive_access.go](backend/internal/bootstrap/exclusive_access.go)），standalone 等待 owner 截止时间过期。收敛失败则拒绝启动，绝不提供混合实例。
- CLI 新增 `--resume`，归档暂存副本支持续跑。

### 兼容语义保持

- 无 `francis.bin` 的旧归档：actor 组件完全不动；
- standalone Francis + 携带 actor 数据的归档：在打开 DB/存储/租约**之前**拒绝（cmd 与 service 双层防护）；
- 无故障导入、SQLite↔Postgres 互导、filesystem/database/S3 三种后端行为不变（新增 20+ 用例覆盖 happy path、三类故障、崩溃后重启回滚/前向、resume、fencing、FK 完整性、隔离暂存区）。
