# Calendar runtime pagination — review редакции 6

2026-11-17. Reviewer: `gpt-6-astra`, high. План: [тот же HTML](../ai-plans/2026-11-16-calendar-synthetic-pagination-pilot.html#revision-6). Координатор сохранил результат review в документе; reviewer не менял файлы, не запускал тесты, providers или внешние вызовы.

**Итог: READY FOR APPROVAL.** Все замечания P1/P2 закрыты на уровне плана. Блокирующих замечаний перед отдельным approval R0–R4 нет. Это не одобрение реализации и не подтверждение correctness/готовности runtime или выкладки.

## Замечания и принятые решения

1. **P1 — Google reconnect A/B.** В текущем `internal/connections/service.go:65` replacement без refresh token наследует предыдущий. В плане Google replacement без собственного RT отклоняется до изменения работающего A, требуется re-consent. Нельзя смешать AT аккаунта B и RT аккаунта A внутри одного нового epoch. Добавлены реальные callback/TokenSource fixtures; Microsoft behavior не расширяется этим изменением.
2. **P1 — успешный HTTP200 с изменением роли.** `internal/google/event_sync.go:61` сейчас игнорирует generated `Events.AccessRole`. Пустой incremental response после owner→reader может оставить private title вне Items. План закрывает старый receipt/generation до применения страницы при несовместимой role/visibility; missing role не превращается в proof. Свежие provider-filtered данные выдаются после проверки/refetch; тест включает старый private title вне списка изменений.
3. **P1 — конкурентный refresh.** Независимые API/worker TokenSource могут выполнить внешнюю ротацию до локального CAS. Убрано обещание lossless concurrent rotation. При недействительном CAS winner — closed/reconnect, без обхода expected epoch/cipher. Поздние401/invalid_grant от старой epoch или вытесненного token не закрывают новую действующую авторизацию. Добавлены двухпроцессные/двух-Store fixtures.
4. **P2 — уже работающий API registry.** План явно задаёт refresh-on-resolve для platform Google по актуальным epoch/token binding, construction из текущего snapshot и conditional replace в существующем registry. Покрыты single/list/fan-out; никакого replay уже начатой mutation, второго provider cache или watcher. Тест соединяет работающий API registry и отдельный worker Store.
5. **P2 — смысл ошибок зависит от операции.** Read source denial и calendar-scoped List404 закрывают source, event Get404 не закрывает calendar. Известный mutation-only403 не отзывает действующее read право; unknown403 conservatively закрывает только source с явным reconnect recovery. Old errors не применяются к новой revision.
6. **Будущая активация.** Startup schema check не останавливает уже работающие старые workers. Перед migration/reader activation нужно quiesce все старые writers; mixed rolling activation не допускается. Это условие отдельной будущей выкладки, не действие текущего этапа и не новая release инфраструктура.

Астра приняла one-shot legacy bootstrap с явной ценой доступности: после failure/cancel/crash восстановление через существующий reconnect, без нового discovery retry scheduler. Тесты настоящего MCP SDK/HTTP, private caller context и авторизация каждого запроса в существующей сессии конкретны и исполнимы. Принятый пользователем небольшой PG p95 overrun пилота не является blocker плана.

## Граница результата

Ревью было двухэтапным: сначала документ и настоящие owning sources, затем только изменённые формулировки. Implementation agents не запускались; read-only Terra составила inventory. Runtime, migrations, dependencies, providers, реальные БД, Gateway/ACL, Git publication и rollout не менялись. Следующий разрешаемый отдельно шаг — реализация R0–R4 с fake HTTP и disposable databases; реальные provider-вызовы/выкладка не входят даже в это предполагаемое approval.
