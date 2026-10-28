Migrations of the service: `NNN_name.up.sql` + `NNN_name.down.sql`, numbered from `000001`.
Rules — `docs/migrations.md`.

000005 (FIN-364) creates independent pre-transfer consent receipts and provider bindings,
adds durable dispatch/deadline fields and finite busy facts. It invalidates pre-evidence
pending/results without revoking persisted session chains. Down removes the new schema,
never restores old authentication secrets. Pause phone Start on all old replicas during
cutover; do not downgrade to repeat-confirm recovery. A downgrade must first exclude
provider/call ID collisions across campaigns, because the previous unique key lacked
campaign. Receipts have no account/request cascade; only their own retention cleanup
removes provider bindings. There are no CHECKs, triggers, functions or extensions.

Down000005 под exclusive table lock отказывает при любом сохранённом receipt; откат пустой схемы проходит up/down/up. После ошибки source dirty проверяется оператором, evidence не удаляется.
