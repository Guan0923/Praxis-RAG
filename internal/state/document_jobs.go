package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Document job states (document_jobs.py).
const (
	DocumentJobQueued     = "queued"
	DocumentJobProcessing = "processing"
	DocumentJobSucceeded  = "succeeded"
	DocumentJobFailed     = "failed"

	// MaxDocumentJobFileBytes is the async admission file ceiling.
	MaxDocumentJobFileBytes = 10 * 1024 * 1024

	// MaxDocumentJobAttempts is the maximum number of executions of one
	// document job: the first execution plus at most two automatic retries.
	// attempts lives in MySQL and therefore survives a new Kafka delivery, a
	// different worker process and a process restart.
	MaxDocumentJobAttempts = 3
)

var documentJobActiveStatuses = []string{DocumentJobQueued, DocumentJobProcessing}
var documentJobTerminalStatuses = []string{DocumentJobSucceeded, DocumentJobFailed}

var documentNamespacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

var supportedDocumentExtensions = map[string]struct{}{
	".doc": {}, ".docx": {}, ".md": {}, ".pdf": {}, ".txt": {},
}

// DocumentValidationError mirrors KnowledgeValidationError for asynchronous
// admission; the runtime maps it to its public validation error.
type DocumentValidationError struct{ Message string }

// Error implements error.
func (e *DocumentValidationError) Error() string { return e.Message }

func newDocumentValidationError(message string) *DocumentValidationError {
	return &DocumentValidationError{Message: message}
}

// DocumentJobSettings mirrors the document_jobs.py env settings.
type DocumentJobSettings struct {
	LeaseSeconds             float64
	HeartbeatSeconds         float64
	SweepAfterSeconds        float64
	TerminalRetentionSeconds float64
}

// DefaultDocumentJobSettings returns the Python defaults.
func DefaultDocumentJobSettings() DocumentJobSettings {
	return DocumentJobSettings{
		LeaseSeconds:             300,
		HeartbeatSeconds:         20,
		SweepAfterSeconds:        120,
		TerminalRetentionSeconds: 30 * 24 * 3600,
	}
}

// DocumentJobSettingsFromEnv mirrors the four positive float settings.
func DocumentJobSettingsFromEnv() (DocumentJobSettings, error) {
	settings := DefaultDocumentJobSettings()
	values := []struct {
		name     string
		fallback float64
		target   *float64
	}{
		{"RAG_DOCUMENT_JOB_LEASE_SECONDS", settings.LeaseSeconds, &settings.LeaseSeconds},
		{"RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS", settings.HeartbeatSeconds, &settings.HeartbeatSeconds},
		{"RAG_DOCUMENT_JOB_SWEEP_SECONDS", settings.SweepAfterSeconds, &settings.SweepAfterSeconds},
		{"RAG_DOCUMENT_JOB_RETENTION_SECONDS", settings.TerminalRetentionSeconds, &settings.TerminalRetentionSeconds},
	}
	for _, item := range values {
		value, err := positiveFloatEnv(item.name, item.fallback)
		if err != nil {
			return DocumentJobSettings{}, err
		}
		*item.target = value
	}
	return settings, nil
}

func positiveFloatEnv(name string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0, newValueError(name + " must be positive")
	}
	return value, nil
}

func canonicalArtifactID(value string) (string, error) {
	if !canonicalUUID(value) {
		return "", newValueError("artifact_id must be a canonical UUID")
	}
	return value, nil
}

func validateDocumentName(name string) error {
	sanitized := windowsSanitizeName(name)
	if sanitized != name || windowsReservedName(name) {
		return newValueError("Invalid document filename or file type")
	}
	extension := strings.ToLower(filepath.Ext(name))
	if _, supported := supportedDocumentExtensions[extension]; !supported {
		return newValueError("Invalid document filename or file type")
	}
	return nil
}

var windowsReservedCharacters = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func windowsSanitizeName(raw string) string {
	name := windowsReservedCharacters.ReplaceAllString(raw, "_")
	name = strings.Trim(name, " .")
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func windowsReservedName(name string) bool {
	base := name
	if index := strings.Index(base, "."); index >= 0 {
		base = base[:index]
	}
	upper := strings.ToUpper(strings.TrimSpace(base))
	switch upper {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(upper) == 4 {
		if strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT") {
			digit := upper[3]
			return digit >= '1' && digit <= '9'
		}
	}
	return false
}

// DocumentJob is one durable ingest job row.
type DocumentJob struct {
	ArtifactID           string  `json:"artifact_id"`
	Namespace            string  `json:"namespace"`
	Name                 string  `json:"name"`
	NameKey              string  `json:"name_key"`
	Content              []byte  `json:"content,omitempty"`
	Size                 int64   `json:"size"`
	SHA256               string  `json:"sha256"`
	ConfigurationVersion int64   `json:"configuration_version"`
	IdempotencyKey       *string `json:"idempotency_key"`
	Status               string  `json:"status"`
	Stage                string  `json:"stage"`
	Attempts             int     `json:"attempts"`
	Owner                *string `json:"owner"`
	ExecutionID          *string `json:"execution_id"`
	LeaseUntil           float64 `json:"lease_until"`
	OperationID          *string `json:"operation_id"`
	RetryAt              float64 `json:"retry_at"`
	Error                *string `json:"error"`
	CreatedAt            float64 `json:"created_at"`
	UpdatedAt            float64 `json:"updated_at"`
	// ScheduleVersion is the scheduling generation Debezium watches. It starts
	// at 1 and is incremented inside the transaction that requeues the job.
	ScheduleVersion int64 `json:"schedule_version"`
}

// DocumentJobTask couples the leased job row with the file bytes read back.
type DocumentJobTask struct {
	DocumentJob
	Content []byte
}

// DocumentAdmission is the submit() result.
type DocumentAdmission struct {
	ArtifactID string `json:"artifact_id"`
	Status     string `json:"status"`
	Reused     bool   `json:"reused"`
}

// DocumentJobStatus is the status() projection.
type DocumentJobStatus struct {
	ArtifactID string  `json:"artifact_id"`
	Status     string  `json:"status"`
	Stage      string  `json:"stage"`
	Error      *string `json:"error"`
}

// DocumentJobs is the repository for durable ingest jobs. MySQL is the source
// of truth; Debezium delivers the identifier-only notifications and no
// transactional outbox is written.
type DocumentJobs struct {
	stores   *Stores
	Settings DocumentJobSettings
	Clock    func() float64
}

// NewDocumentJobs builds the repository; invalid RAG_DOCUMENT_* env fails like
// the Python module-level helpers.
func NewDocumentJobs(stores *Stores) (*DocumentJobs, error) {
	settings, err := DocumentJobSettingsFromEnv()
	if err != nil {
		return nil, err
	}
	return &DocumentJobs{stores: stores, Settings: settings, Clock: unixSeconds}, nil
}

func (d *DocumentJobs) now() float64 {
	if d.Clock == nil {
		return unixSeconds()
	}
	return d.Clock()
}

const documentJobColumns = "artifact_id, namespace, name, name_key, content, size, sha256, " +
	"configuration_version, idempotency_key, status, stage, attempts, owner, execution_id, " +
	"lease_until, operation_id, retry_at, error, created_at, updated_at, schedule_version"

func scanDocumentJob(scanner rowScanner, includeContent bool) (*DocumentJob, error) {
	var (
		job            DocumentJob
		content        []byte
		idempotencyKey sql.NullString
		owner          sql.NullString
		executionID    sql.NullString
		operationID    sql.NullString
		errorText      sql.NullString
	)
	destinations := []any{&job.ArtifactID, &job.Namespace, &job.Name, &job.NameKey}
	if includeContent {
		destinations = append(destinations, &content)
	} else {
		var ignored []byte
		destinations = append(destinations, &ignored)
	}
	destinations = append(destinations, &job.Size, &job.SHA256, &job.ConfigurationVersion,
		&idempotencyKey, &job.Status, &job.Stage, &job.Attempts, &owner, &executionID,
		&job.LeaseUntil, &operationID, &job.RetryAt, &errorText, &job.CreatedAt, &job.UpdatedAt,
		&job.ScheduleVersion)
	if err := scanner.Scan(destinations...); err != nil {
		return nil, err
	}
	job.Content = content
	job.IdempotencyKey = nullStringPointer(idempotencyKey)
	job.Owner = nullStringPointer(owner)
	job.ExecutionID = nullStringPointer(executionID)
	job.OperationID = nullStringPointer(operationID)
	job.Error = nullStringPointer(errorText)
	return &job, nil
}

func documentJobNameKey(namespace, name string) string {
	return sha256Hex(namespace + ":" + strings.ToLower(name))
}

// Submit persists the complete job in one transaction; the schedule_version
// written with the row is the only notification Debezium produces.
func (d *DocumentJobs) Submit(ctx context.Context, namespace, name string, content []byte, idempotencyKey *string) (DocumentAdmission, error) {
	if err := validateDocumentName(name); err != nil {
		return DocumentAdmission{}, newDocumentValidationError(err.Error())
	}
	if len(content) == 0 || len(content) > MaxDocumentJobFileBytes {
		return DocumentAdmission{}, newDocumentValidationError("File must be non-empty and no larger than 10 MiB")
	}
	if idempotencyKey != nil && (len(*idempotencyKey) < 1 || len(*idempotencyKey) > 128) {
		return DocumentAdmission{}, newDocumentValidationError("idempotency_key must be a string of 1 to 128 characters")
	}
	digest := sha256Sum(content)
	nameKey := documentJobNameKey(namespace, name)
	artifactID, err := uuid4String()
	if err != nil {
		return DocumentAdmission{}, err
	}
	now := d.now()
	admission, insertErr := d.insertJob(ctx, namespace, name, content, idempotencyKey, digest, nameKey, artifactID, now)
	if insertErr == nil {
		return admission, nil
	}
	if !isUniqueViolation(insertErr) {
		return DocumentAdmission{}, insertErr
	}
	existing, lookupErr := d.byIdempotencyKey(ctx, namespace, idempotencyKey)
	if lookupErr != nil {
		return DocumentAdmission{}, lookupErr
	}
	if existing == nil {
		return DocumentAdmission{}, newUploadConflict("Upload admission conflicted; retry with the same idempotency_key")
	}
	if existing.Name != name || existing.SHA256 != digest {
		return DocumentAdmission{}, newUploadConflict("Idempotency key has different file input")
	}
	return DocumentAdmission{ArtifactID: existing.ArtifactID, Status: existing.Status, Reused: true}, nil
}

func (d *DocumentJobs) byIdempotencyKey(ctx context.Context, namespace string, idempotencyKey *string) (*DocumentJob, error) {
	if idempotencyKey == nil {
		return nil, nil
	}
	job, err := scanDocumentJob(d.stores.DB.QueryRowContext(ctx,
		"SELECT "+documentJobColumns+" FROM document_jobs WHERE namespace = ? AND idempotency_key = ?",
		namespace, *idempotencyKey), false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return job, err
}

func (d *DocumentJobs) insertJob(
	ctx context.Context,
	namespace, name string,
	content []byte,
	idempotencyKey *string,
	digest, nameKey, artifactID string,
	now float64,
) (DocumentAdmission, error) {
	transaction, err := d.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return DocumentAdmission{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	// Reuse the journal's persistent lease rows to serialize submissions that
	// share one document name. Different names keep independent rows, so
	// concurrent admissions of different files do not queue behind each other.
	if err := d.stores.Uploads.guardName(ctx, transaction, namespace, name); err != nil {
		return DocumentAdmission{}, err
	}
	var version sql.NullInt64
	query := "SELECT version FROM knowledge_bases WHERE namespace = ?" + d.stores.Uploads.forShare()
	if err := transaction.QueryRowContext(ctx, query, namespace).Scan(&version); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return DocumentAdmission{}, err
	}
	if !version.Valid {
		return DocumentAdmission{}, newValueError("Knowledge base is not configured")
	}
	if idempotencyKey != nil {
		// Read without a row lock: a missing idempotency key would otherwise
		// take a gap lock that deadlocks against a concurrent admission's
		// INSERT into the same index gap. The unique index plus the
		// unique-violation fallback in Submit still make a replay return the
		// original artifact.
		existing, err := scanDocumentJob(transaction.QueryRowContext(ctx,
			"SELECT "+documentJobColumns+" FROM document_jobs WHERE namespace = ? AND idempotency_key = ?",
			namespace, *idempotencyKey), false)
		if err == nil {
			if existing.Name != name || existing.SHA256 != digest {
				return DocumentAdmission{}, newUploadConflict("Idempotency key has different file input")
			}
			return DocumentAdmission{ArtifactID: existing.ArtifactID, Status: existing.Status, Reused: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return DocumentAdmission{}, err
		}
	}
	rows, err := transaction.QueryContext(ctx,
		"SELECT name FROM documents WHERE namespace = ? AND del_flag = 0", namespace)
	if err != nil {
		return DocumentAdmission{}, err
	}
	for rows.Next() {
		var published string
		if err := rows.Scan(&published); err != nil {
			_ = rows.Close()
			return DocumentAdmission{}, err
		}
		if strings.EqualFold(published, name) {
			_ = rows.Close()
			return DocumentAdmission{}, newDocumentExistsConflict("Document already exists; use update_document")
		}
	}
	if err := rows.Close(); err != nil {
		return DocumentAdmission{}, err
	}
	var pending string
	err = transaction.QueryRowContext(ctx,
		"SELECT artifact_id FROM document_jobs WHERE namespace = ? AND name_key = ? "+
			"AND status IN (?, ?) LIMIT 1", namespace, nameKey, DocumentJobQueued, DocumentJobProcessing).
		Scan(&pending)
	if err == nil {
		return DocumentAdmission{}, newDocumentQueuedConflict("A document with this filename is already queued")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DocumentAdmission{}, err
	}
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO document_jobs (artifact_id, namespace, name, name_key, content, size, sha256, "+
			"configuration_version, idempotency_key, status, stage, attempts, owner, execution_id, "+
			"lease_until, operation_id, retry_at, error, created_at, updated_at, schedule_version) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, NULL, 0, NULL, ?, NULL, ?, ?, 1)",
		artifactID, namespace, name, nameKey, content, int64(len(content)), digest, version.Int64,
		idempotencyKey, DocumentJobQueued, DocumentJobQueued, now, now, now); err != nil {
		return DocumentAdmission{}, err
	}
	if err := transaction.Commit(); err != nil {
		return DocumentAdmission{}, err
	}
	return DocumentAdmission{
		ArtifactID: artifactID,
		Status:     DocumentJobQueued,
		Reused:     false,
	}, nil
}

// Get returns one job, optionally constrained to a namespace.
func (d *DocumentJobs) Get(ctx context.Context, artifactID string, namespace *string) (*DocumentJob, error) {
	if _, err := canonicalArtifactID(artifactID); err != nil {
		return nil, err
	}
	query := "SELECT " + documentJobColumns + " FROM document_jobs WHERE artifact_id = ?"
	arguments := []any{artifactID}
	if namespace != nil {
		query += " AND namespace = ?"
		arguments = append(arguments, *namespace)
	}
	job, err := scanDocumentJob(d.stores.DB.QueryRowContext(ctx, query, arguments...), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return job, err
}

// Status returns the public status projection.
func (d *DocumentJobs) Status(ctx context.Context, namespace, artifactID string) (DocumentJobStatus, error) {
	job, err := d.Get(ctx, artifactID, &namespace)
	if err != nil {
		return DocumentJobStatus{}, err
	}
	if job == nil {
		return DocumentJobStatus{}, newLookupError("Document upload not found")
	}
	return DocumentJobStatus{ArtifactID: job.ArtifactID, Status: job.Status, Stage: job.Stage, Error: job.Error}, nil
}

// Terminal reports whether the job is unknown, invalid, or finished.
func (d *DocumentJobs) Terminal(ctx context.Context, artifactID string) (bool, error) {
	if _, err := canonicalArtifactID(artifactID); err != nil {
		return true, nil
	}
	job, err := d.Get(ctx, artifactID, nil)
	if err != nil {
		return false, err
	}
	if job == nil {
		return true, nil
	}
	for _, terminal := range documentJobTerminalStatuses {
		if job.Status == terminal {
			return true, nil
		}
	}
	return false, nil
}

// Claim takes the execution lease for one schema-4 notification. Returning
// (nil, nil) means the delivery is safely skippable: the row is missing,
// terminal, owned by a live lease, or carries a different schedule_version.
func (d *DocumentJobs) Claim(ctx context.Context, notification DocumentNotification) (*DocumentJobTask, error) {
	validated, err := ValidateDocumentNotification(map[string]any{
		"schema": float64(DocumentEnvelopeSchema), "artifact_id": notification.ArtifactID,
		"namespace": notification.Namespace, "schedule_version": notification.ScheduleVersion,
	})
	if err != nil {
		return nil, err
	}
	now := d.now()
	owner, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	executionID, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	transaction, err := d.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	job, err := scanDocumentJob(transaction.QueryRowContext(ctx,
		"SELECT "+documentJobColumns+" FROM document_jobs WHERE artifact_id = ? AND namespace = ?"+
			d.stores.Uploads.forUpdate(), validated.ArtifactID, validated.Namespace), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if job.ScheduleVersion != validated.ScheduleVersion || !documentJobActiveStatus(job.Status) {
		return nil, nil
	}
	if job.Status == DocumentJobProcessing && job.LeaseUntil > now {
		return nil, nil
	}
	// A retry is only claimable once retry_at has arrived. Debezium may replay
	// an old snapshot row before the periodic sweep emits the due retry.
	if job.Stage == "retry_wait" && job.RetryAt > now {
		return nil, nil
	}
	if job.Attempts >= MaxDocumentJobAttempts {
		// Never start a fourth execution. Terminalize in this transaction so a
		// duplicate notification, a sweep or a lease-expiry recovery all stop on
		// the same row with the last recorded reason.
		if err := failAttemptsExhausted(ctx, transaction, job, now); err != nil {
			return nil, err
		}
		if err := transaction.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	attempts := job.Attempts + 1
	leaseUntil := now + d.Settings.LeaseSeconds
	if _, err := transaction.ExecContext(ctx,
		"UPDATE document_jobs SET status = ?, stage = ?, owner = ?, execution_id = ?, lease_until = ?, "+
			"attempts = ?, error = NULL, updated_at = ? WHERE artifact_id = ?",
		DocumentJobProcessing, "receiving", owner, executionID, leaseUntil, attempts, now, job.ArtifactID); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	job.Status = DocumentJobProcessing
	job.Stage = "receiving"
	job.Owner = &owner
	job.ExecutionID = &executionID
	job.LeaseUntil = leaseUntil
	job.Attempts = attempts
	job.UpdatedAt = now
	return &DocumentJobTask{DocumentJob: *job, Content: job.Content}, nil
}

// documentJobActiveStatus reports whether the status can still be executed.
func documentJobActiveStatus(status string) bool {
	for _, active := range documentJobActiveStatuses {
		if status == active {
			return true
		}
	}
	return false
}

// failAttemptsExhausted writes the terminal row for a job that already used all
// executions. The caller owns the transaction and the row lock.
func failAttemptsExhausted(ctx context.Context, transaction *sql.Tx, job *DocumentJob, now float64) error {
	message := attemptLimitMessage(job)
	_, err := transaction.ExecContext(ctx,
		"UPDATE document_jobs SET status = ?, stage = ?, error = ?, owner = NULL, execution_id = NULL, "+
			"lease_until = 0, retry_at = 0, content = NULL, updated_at = ? WHERE artifact_id = ?",
		DocumentJobFailed, "failed", message, now, job.ArtifactID)
	return err
}

// attemptLimitMessage keeps the last recorded failure reason when the previous
// attempt recorded one; otherwise it names the exhausted execution budget.
func attemptLimitMessage(job *DocumentJob) string {
	message := fmt.Sprintf("Document processing failed: attempt limit reached (%d attempts)",
		MaxDocumentJobAttempts)
	if job != nil && job.Error != nil {
		if lastError := strings.TrimSpace(*job.Error); lastError != "" {
			message += "; last error: " + lastError
		}
	}
	return message
}

// Heartbeat renews the processing lease of this execution.
func (d *DocumentJobs) Heartbeat(ctx context.Context, task *DocumentJobTask) error {
	if task.Owner == nil || task.ExecutionID == nil {
		return newUploadConflict("Document execution is no longer current")
	}
	_, err := d.stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET lease_until = ? WHERE artifact_id = ? AND execution_id = ? AND status = ?",
		d.now()+d.Settings.LeaseSeconds, task.ArtifactID, *task.ExecutionID, DocumentJobProcessing)
	return err
}

// Progress records the current stage and renews the lease; a lost execution
// lease is an UploadConflict.
func (d *DocumentJobs) Progress(ctx context.Context, task *DocumentJobTask, stage string) error {
	if task.ExecutionID == nil {
		return newUploadConflict("Document execution is no longer current")
	}
	now := d.now()
	result, err := d.stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET stage = ?, operation_id = ?, lease_until = ?, updated_at = ? "+
			"WHERE artifact_id = ? AND execution_id = ? AND status = ?",
		stage, nilIfNil(task.OperationID), now+d.Settings.LeaseSeconds, now,
		task.ArtifactID, *task.ExecutionID, DocumentJobProcessing)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return newUploadConflict("Document execution is no longer current")
	}
	task.Stage = stage
	return nil
}

// Bind links the durable upload operation to this task; progress() persists it.
func (d *DocumentJobs) Bind(task *DocumentJobTask, operation *UploadOperation) {
	task.OperationID = &operation.ID
}

// ValidatePublication checks the knowledge-base configuration version inside the
// caller's upload transaction.
func (d *DocumentJobs) ValidatePublication(ctx context.Context, tx *sql.Tx, task *DocumentJobTask) error {
	var version sql.NullInt64
	query := "SELECT version FROM knowledge_bases WHERE namespace = ?" + d.stores.Uploads.forUpdate()
	if err := tx.QueryRowContext(ctx, query, task.Namespace).Scan(&version); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !version.Valid || version.Int64 != task.ConfigurationVersion {
		return newUploadConflict("Knowledge-base configuration changed; upload again")
	}
	return nil
}

func (d *DocumentJobs) signedResultValue(result any, key string) any {
	object, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	return object[key]
}

// CommittedOperation finds the committed journal publication of this task.
func (d *DocumentJobs) CommittedOperation(ctx context.Context, task *DocumentJobTask) (*UploadOperation, error) {
	prefix := "document-task:" + task.ArtifactID + ":"
	var operationID string
	err := d.stores.DB.QueryRowContext(ctx,
		"SELECT id FROM upload_operations WHERE namespace = ? AND status = ? "+
			"AND idempotency_key LIKE ? ESCAPE '!' ORDER BY created_at LIMIT 1",
		task.Namespace, UploadCommitted, escapeLike(prefix)+"%").
		Scan(&operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	operation, err := d.stores.Uploads.Get(ctx, operationID)
	if err != nil {
		return nil, err
	}
	nameMatches := operation.Name == task.Name
	shaMatches := d.signedResultValue(operation.Result, "sha256") == task.SHA256
	sizeMatches := numericValue(d.signedResultValue(operation.Result, "size")) == float64(task.Size)
	configMatches := numericValue(d.signedResultValue(operation.Result, "task_configuration_version")) ==
		float64(task.ConfigurationVersion)
	if !nameMatches || !shaMatches || !sizeMatches || !configMatches {
		return nil, newUploadConflict("Committed upload input differs")
	}
	return operation, nil
}

func escapeLike(value string) string {
	replacer := strings.NewReplacer(`!`, `!!`, `%`, `!%`, `_`, `!_`)
	return replacer.Replace(value)
}

// Reconcile adopts an already committed journal publication.
func (d *DocumentJobs) Reconcile(ctx context.Context, task *DocumentJobTask) (bool, error) {
	operation, err := d.CommittedOperation(ctx, task)
	if err != nil {
		return false, err
	}
	if operation == nil {
		return false, nil
	}
	if err := d.finish(ctx, task, DocumentJobSucceeded, nil, &operation.ID); err != nil {
		return false, err
	}
	return true, nil
}

// ExecutionBusy reports an unfinished journal execution for this artifact.
func (d *DocumentJobs) ExecutionBusy(ctx context.Context, task *DocumentJobTask) (bool, error) {
	prefix := "document-task:" + task.ArtifactID + ":"
	var operationID string
	err := d.stores.DB.QueryRowContext(ctx,
		"SELECT id FROM upload_operations WHERE namespace = ? AND status IN (?, ?) "+
			"AND idempotency_key LIKE ? ESCAPE '!' LIMIT 1",
		task.Namespace, UploadIntent, UploadRunning, escapeLike(prefix)+"%").Scan(&operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Succeed finishes the job only if the journal publication is committed.
func (d *DocumentJobs) Succeed(ctx context.Context, task *DocumentJobTask) error {
	reconciled, err := d.Reconcile(ctx, task)
	if err != nil {
		return err
	}
	if !reconciled {
		return newUploadConflict("Document publication is not committed")
	}
	return nil
}

func (d *DocumentJobs) finish(ctx context.Context, task *DocumentJobTask, status string, errorMessage *string, operationID *string) error {
	now := d.now()
	transaction, err := d.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	job, err := scanDocumentJob(transaction.QueryRowContext(ctx,
		"SELECT "+documentJobColumns+" FROM document_jobs WHERE artifact_id = ?"+
			d.stores.Uploads.forUpdate(), task.ArtifactID), false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if job.Status == DocumentJobSucceeded {
		return nil
	}
	if status != DocumentJobSucceeded && task.ExecutionID != nil &&
		(job.ExecutionID == nil || *job.ExecutionID != *task.ExecutionID) {
		return newUploadConflict("Document execution is no longer current")
	}
	stage := "failed"
	if status == DocumentJobSucceeded {
		stage = "completed"
	}
	var content any = nil
	updatedOperation := operationID
	if updatedOperation == nil {
		updatedOperation = task.OperationID
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE document_jobs SET status = ?, stage = ?, error = ?, owner = NULL, lease_until = 0, "+
			"content = ?, operation_id = ?, updated_at = ? WHERE artifact_id = ?",
		status, stage, errorMessage, content, nilIfNil(updatedOperation), now, task.ArtifactID); err != nil {
		return err
	}
	return transaction.Commit()
}

// Fail requeues (retry) or terminates the job; committed publications reconcile.
// A retry is only a "waiting room": it records the reason and retry_at without
// touching schedule_version, so Debezium emits nothing yet. The periodic Sweep
// emits exactly one replacement notification when retry_at is due. Once the
// execution budget is spent (or the error is not retryable) the row becomes
// failed with the last reason.
func (d *DocumentJobs) Fail(ctx context.Context, task *DocumentJobTask, retry bool, errorMessage *string) error {
	reconciled, err := d.Reconcile(ctx, task)
	if err != nil {
		return err
	}
	if reconciled {
		return nil
	}
	if !retry || task.Attempts >= MaxDocumentJobAttempts {
		return d.finish(ctx, task, DocumentJobFailed, errorMessage, nil)
	}
	now := d.now()
	if task.ExecutionID == nil {
		return newUploadConflict("Document execution is no longer current")
	}
	_, err = d.stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET status = ?, stage = ?, error = COALESCE(?, error), owner = NULL, "+
			"execution_id = NULL, lease_until = 0, retry_at = ?, updated_at = ? "+
			"WHERE artifact_id = ? AND execution_id = ? AND status = ? AND attempts = ?",
		DocumentJobQueued, "retry_wait", errorMessage, now+d.Settings.SweepAfterSeconds, now,
		task.ArtifactID, *task.ExecutionID, DocumentJobProcessing, task.Attempts)
	return err
}

// DeadLetter terminates an undeliverable job unless a live execution or a
// committed publication exists.
func (d *DocumentJobs) DeadLetter(ctx context.Context, notification DocumentNotification) (bool, error) {
	validated, err := ValidateDocumentNotification(map[string]any{
		"schema": float64(DocumentEnvelopeSchema), "artifact_id": notification.ArtifactID,
		"namespace": notification.Namespace, "schedule_version": notification.ScheduleVersion,
	})
	if err != nil {
		return false, err
	}
	job, err := d.Get(ctx, validated.ArtifactID, &validated.Namespace)
	if err != nil {
		return false, err
	}
	if job == nil {
		return true, nil
	}
	for _, terminal := range documentJobTerminalStatuses {
		if job.Status == terminal {
			return true, nil
		}
	}
	if job.Status == DocumentJobProcessing && job.LeaseUntil > d.now() {
		return false, nil
	}
	task := &DocumentJobTask{DocumentJob: *job}
	reconciled, err := d.Reconcile(ctx, task)
	if err != nil {
		return false, err
	}
	if reconciled {
		return true, nil
	}
	busy, err := d.ExecutionBusy(ctx, task)
	if err != nil {
		return false, err
	}
	if busy {
		return false, nil
	}
	// Keep the last recorded attempt reason in the terminal text: broker retry
	// exhaustion alone ("Message retry limit reached") tells operators nothing
	// about whether Qdrant, the model API or parsing failed.
	message := "Message retry limit reached"
	if job.Error != nil {
		if lastError := strings.TrimSpace(*job.Error); lastError != "" {
			message += "; last error: " + lastError
		}
	}
	if err := d.finish(ctx, task, DocumentJobFailed, &message, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Sweep emits the replacement notification for due retries and requeues jobs
// whose lease expired, incrementing schedule_version in the same UPDATE so
// Debezium emits exactly one consumable change per requeue:
//
//   - queued retry_wait jobs whose retry_at is due and whose row has been
//     quiet for one sweep window (the only automatic retry path);
//   - processing jobs whose lease expired, because the worker crashed.
//
// A job that is merely queued is intentionally left alone: waiting for a worker
// or a parse slot must neither bump schedule_version nor consume an execution
// attempt. Jobs that already used all executions become terminal instead of
// being requeued, so no fourth execution can start.
//
// No outbox row is written; the schedule_version change is the only signal.
func (d *DocumentJobs) Sweep(ctx context.Context, limit int) (int, error) {
	return d.sweep(ctx, limit, false)
}

// RequeueDueNow is the operator-triggered variant of Sweep: it requeues due
// retry_wait jobs and lease-expired processing jobs immediately, ignoring the
// one-sweep-window quiet rule for retry_wait rows. It exists for the Console
// "立即重投" action so an operator does not have to wait for the periodic
// sweep; the durability contract is identical to Sweep (schedule_version is
// incremented in the same UPDATE, so Debezium emits exactly one replacement
// notification, and the attempt budget / terminalization rules are shared).
func (d *DocumentJobs) RequeueDueNow(ctx context.Context, limit int) (int, error) {
	return d.sweep(ctx, limit, true)
}

// sweep implements both variants. immediate skips the retry_wait quiet rule
// (the lease-expiry path never had it).
func (d *DocumentJobs) sweep(ctx context.Context, limit int, immediate bool) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	now := d.now()
	cutoff := now - d.Settings.SweepAfterSeconds
	transaction, err := d.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = transaction.Rollback() }()
	var retryDue []*DocumentJob
	if immediate {
		retryDue, err = d.selectJobsForSweep(ctx, transaction,
			"status = ? AND stage = ? AND retry_at <= ? ORDER BY retry_at LIMIT ?"+d.stores.Uploads.forUpdate(),
			DocumentJobQueued, "retry_wait", now, limit)
	} else {
		retryDue, err = d.selectJobsForSweep(ctx, transaction,
			"status = ? AND stage = ? AND retry_at <= ? AND updated_at <= ? ORDER BY retry_at LIMIT ?"+d.stores.Uploads.forUpdate(),
			DocumentJobQueued, "retry_wait", now, cutoff, limit)
	}
	if err != nil {
		return 0, err
	}
	expired, err := d.selectJobsForSweep(ctx, transaction,
		"status = ? AND lease_until <= ? ORDER BY created_at LIMIT ?"+d.stores.Uploads.forUpdate(),
		DocumentJobProcessing, now, limit)
	if err != nil {
		return 0, err
	}
	requeued := 0
	for _, job := range append(retryDue, expired...) {
		if job.Attempts >= MaxDocumentJobAttempts {
			if err := failAttemptsExhausted(ctx, transaction, job, now); err != nil {
				return 0, err
			}
			continue
		}
		if _, err := transaction.ExecContext(ctx,
			"UPDATE document_jobs SET schedule_version = schedule_version + 1, status = ?, stage = ?, "+
				"owner = NULL, execution_id = NULL, lease_until = 0, retry_at = 0, updated_at = ? "+
				"WHERE artifact_id = ?",
			DocumentJobQueued, "retry_wait", now, job.ArtifactID); err != nil {
			return 0, err
		}
		requeued++
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return requeued, nil
}

func (d *DocumentJobs) selectJobsForSweep(ctx context.Context, transaction *sql.Tx, where string, arguments ...any) ([]*DocumentJob, error) {
	query := "SELECT " + documentJobColumns + " FROM document_jobs WHERE " + where
	rows, err := transaction.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	jobs := make([]*DocumentJob, 0)
	for rows.Next() {
		job, err := scanDocumentJob(rows, false)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// GCJobs deletes terminal jobs older than the retention window.
func (d *DocumentJobs) GCJobs(ctx context.Context, retainSeconds *float64) (int, error) {
	retention := d.Settings.TerminalRetentionSeconds
	if retainSeconds != nil {
		retention = *retainSeconds
	}
	result, err := d.stores.DB.ExecContext(ctx,
		"DELETE FROM document_jobs WHERE status IN (?, ?) AND updated_at < ?",
		DocumentJobSucceeded, DocumentJobFailed, d.now()-retention)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// numericValue normalizes JSON-decoded numbers for comparisons.
func numericValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return math.NaN()
		}
		return parsed
	default:
		return math.NaN()
	}
}

func finiteNumericValue(value any) (float64, bool) {
	parsed := numericValue(value)
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}
