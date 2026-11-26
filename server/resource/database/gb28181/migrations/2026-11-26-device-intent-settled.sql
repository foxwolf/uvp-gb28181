-- 已发出的设备操作意图增加终态 settled。cancelled 仍只表示从未下发的 reserved。
-- 新基线已经带这条检查；本脚本只改存量库，且可重复执行。
SET @device_intent_phase_drop := (
  SELECT IFNULL(MAX(IF(CHECK_CLAUSE LIKE '%settled%', 'SELECT 1', 'ALTER TABLE `gb_device_operation_intent` DROP CHECK `ck_device_intent_phase`')), 'SELECT 1')
  FROM information_schema.CHECK_CONSTRAINTS
  WHERE CONSTRAINT_SCHEMA = DATABASE() AND CONSTRAINT_NAME = 'ck_device_intent_phase'
);
PREPARE device_intent_phase_drop_stmt FROM @device_intent_phase_drop;
EXECUTE device_intent_phase_drop_stmt;
DEALLOCATE PREPARE device_intent_phase_drop_stmt;

SET @device_intent_phase_add := (
  SELECT IFNULL(MAX('SELECT 1'), 'ALTER TABLE `gb_device_operation_intent` ADD CONSTRAINT `ck_device_intent_phase` CHECK (((`updated_at` >= `created_at`) and (((`state` = _ascii''reserved'') and (`row_version` = 1) and (`dispatch_started_at` is null) and (`cancelled_at` is null)) or ((`state` = _ascii''dispatched'') and (`row_version` >= 2) and (`cancelled_at` is null) and (`dispatch_started_at` is not null) and (`dispatch_started_at` >= `created_at`) and (`dispatch_started_at` <= `updated_at`)) or ((`state` = _ascii''cancelled'') and (`row_version` >= 2) and (`dispatch_started_at` is null) and (`cancelled_at` is not null) and (`cancelled_at` >= `created_at`) and (`cancelled_at` <= `updated_at`)) or ((`state` = _ascii''settled'') and (`row_version` >= 3) and (`cancelled_at` is null) and (`dispatch_started_at` is not null) and (`dispatch_started_at` >= `created_at`) and (`dispatch_started_at` <= `updated_at`)))))')
  FROM information_schema.CHECK_CONSTRAINTS
  WHERE CONSTRAINT_SCHEMA = DATABASE() AND CONSTRAINT_NAME = 'ck_device_intent_phase'
);
PREPARE device_intent_phase_add_stmt FROM @device_intent_phase_add;
EXECUTE device_intent_phase_add_stmt;
DEALLOCATE PREPARE device_intent_phase_add_stmt;
