package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"github.com/rishabhm/universal-sql-query-layer/internal/models"
	qerrors "github.com/rishabhm/universal-sql-query-layer/pkg/errors"
	"github.com/rishabhm/universal-sql-query-layer/pkg/middleware"
	"github.com/rishabhm/universal-sql-query-layer/pkg/tracing"
)

type parser interface {
	ParseSQL(sql string) (models.QueryPlan, error)
}

type executor interface {
	Execute(ctx context.Context, principal *models.Principal, plan models.QueryPlan, req models.QueryRequest) (models.QueryResponse, error)
}

type Handler struct {
	parser   parser
	executor executor
	logger   *zap.Logger
}

func NewHandler(parser parser, executor executor, logger *zap.Logger) *Handler {
	return &Handler{
		parser:   parser,
		executor: executor,
		logger:   logger,
	}
}

func (h *Handler) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) Query(w http.ResponseWriter, r *http.Request) {
	tracer := otel.Tracer(tracing.ServiceName)
	ctx, span := tracer.Start(r.Context(), "gateway.query")
	defer span.End()

	traceID := uuid.NewString()
	span.SetAttributes(attribute.String("query.trace_id", traceID))

	principal, ok := middleware.PrincipalFromContext(ctx)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"code":     "UNAUTHORIZED",
			"message":  "missing principal in auth context",
			"trace_id": traceID,
		})
		return
	}

	var req models.QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, qerrors.New(qerrors.CodeInvalidQuery, "invalid request body", "", 0, err), traceID)
		return
	}
	if req.SQL == "" {
		h.writeError(w, qerrors.New(qerrors.CodeInvalidQuery, "sql is required", "", 0, nil), traceID)
		return
	}

	span.SetAttributes(
		attribute.String("query.sql", req.SQL),
		attribute.String("tenant.id", principal.TenantID),
	)

	plan, err := h.parser.ParseSQL(req.SQL)
	if err != nil {
		h.writeError(w, err, traceID)
		return
	}
	if req.MaxStalenessMS != nil {
		plan.MaxStalenessMS = *req.MaxStalenessMS
	}

	resp, err := h.executor.Execute(ctx, principal, plan, req)
	if err != nil {
		span.RecordError(err)
		h.writeError(w, err, traceID)
		return
	}
	resp.TraceID = traceID

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) writeError(w http.ResponseWriter, err error, traceID string) {
	var qErr *qerrors.QueryError
	if !errors.As(err, &qErr) {
		h.logger.Error("unhandled gateway error", zap.Error(err), zap.String("trace_id", traceID))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code":     "INTERNAL_ERROR",
			"message":  "internal server error",
			"trace_id": traceID,
		})
		return
	}

	if qErr.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(qErr.RetryAfter.Seconds()), 10))
	}

	writeJSON(w, qErr.HTTPStatus(), map[string]any{
		"code":     qErr.Code,
		"message":  qErr.Message,
		"source":   qErr.Source,
		"trace_id": traceID,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
