package service

import (
	"context"
	"testing"
	"time"

	"github.com/bridgecore/bridgecore/internal/models"
	"github.com/bridgecore/bridgecore/internal/repository"
)

type fakeUsageStore struct {
	logs   []*models.UsageLog
	nextID int
}

func newFakeUsageStore() *fakeUsageStore {
	return &fakeUsageStore{}
}

func (f *fakeUsageStore) Create(ctx context.Context, u *models.UsageLog) error {
	f.nextID++
	u.ID = "usage-" + itoa(f.nextID)
	u.CreatedAt = time.Now()
	f.logs = append(f.logs, u)
	return nil
}

func (f *fakeUsageStore) ListByTenant(ctx context.Context, tenantID, endpointFilter, methodFilter string, from, to *time.Time, page, pageSize int) ([]*models.UsageLog, int64, error) {
	var matched []*models.UsageLog
	for _, l := range f.logs {
		if l.TenantID == nil || *l.TenantID != tenantID {
			continue
		}
		if endpointFilter != "" && l.Endpoint != endpointFilter {
			continue
		}
		if methodFilter != "" && l.Method != methodFilter {
			continue
		}
		matched = append(matched, l)
	}
	total := int64(len(matched))

	start := (page - 1) * pageSize
	if start > len(matched) {
		start = len(matched)
	}
	end := start + pageSize
	if end > len(matched) {
		end = len(matched)
	}
	return matched[start:end], total, nil
}

func (f *fakeUsageStore) SummaryByTenant(ctx context.Context, tenantID string, from, to *time.Time) ([]repository.EndpointSummary, error) {
	type key struct{ endpoint, method string }
	agg := map[key]*repository.EndpointSummary{}

	for _, l := range f.logs {
		if l.TenantID == nil || *l.TenantID != tenantID {
			continue
		}
		k := key{l.Endpoint, l.Method}
		s, ok := agg[k]
		if !ok {
			s = &repository.EndpointSummary{Endpoint: l.Endpoint, Method: l.Method}
			agg[k] = s
		}
		s.RequestCount++
		if l.StatusCode >= 400 {
			s.ErrorCount++
		}
		s.AvgLatencyMS = (s.AvgLatencyMS*float64(s.RequestCount-1) + float64(l.LatencyMS)) / float64(s.RequestCount)
	}

	var out []repository.EndpointSummary
	for _, s := range agg {
		out = append(out, *s)
	}
	return out, nil
}

func newTestUsageService() *UsageService {
	return NewUsageService(newFakeUsageStore())
}

func strPtr(s string) *string { return &s }

func TestUsageService_Record_PersistsUsageLog(t *testing.T) {
	svc := newTestUsageService()
	tenantID := "tenant-1"

	err := svc.Record(context.Background(), RecordUsageInput{
		TenantID: &tenantID, Endpoint: "/api/v1/tenants", Method: "GET", StatusCode: 200, LatencyMS: 42, RequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	logs, total, err := svc.List(context.Background(), tenantID, "", "", nil, nil, 1, 20)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("expected 1 usage log, got total=%d len=%d", total, len(logs))
	}
	if logs[0].Endpoint != "/api/v1/tenants" || logs[0].StatusCode != 200 {
		t.Fatalf("unexpected usage log contents: %+v", logs[0])
	}
}

func TestUsageService_List_FiltersByEndpointAndMethod(t *testing.T) {
	svc := newTestUsageService()
	ctx := context.Background()
	tenantID := "tenant-1"

	_ = svc.Record(ctx, RecordUsageInput{TenantID: &tenantID, Endpoint: "/api/v1/tenants", Method: "GET", StatusCode: 200, LatencyMS: 10, RequestID: "r1"})
	_ = svc.Record(ctx, RecordUsageInput{TenantID: &tenantID, Endpoint: "/api/v1/users", Method: "POST", StatusCode: 201, LatencyMS: 20, RequestID: "r2"})
	_ = svc.Record(ctx, RecordUsageInput{TenantID: &tenantID, Endpoint: "/api/v1/tenants", Method: "POST", StatusCode: 201, LatencyMS: 15, RequestID: "r3"})

	logs, total, err := svc.List(ctx, tenantID, "/api/v1/tenants", "", nil, nil, 1, 20)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if total != 2 {
		t.Fatalf("expected 2 matching logs for endpoint filter, got %d", total)
	}
	for _, l := range logs {
		if l.Endpoint != "/api/v1/tenants" {
			t.Fatalf("unexpected endpoint in filtered results: %s", l.Endpoint)
		}
	}
}

func TestUsageService_List_IsolatesByTenant(t *testing.T) {
	svc := newTestUsageService()
	ctx := context.Background()

	tenantA, tenantB := "tenant-a", "tenant-b"
	_ = svc.Record(ctx, RecordUsageInput{TenantID: &tenantA, Endpoint: "/x", Method: "GET", StatusCode: 200, LatencyMS: 5, RequestID: "r1"})
	_ = svc.Record(ctx, RecordUsageInput{TenantID: &tenantB, Endpoint: "/x", Method: "GET", StatusCode: 200, LatencyMS: 5, RequestID: "r2"})

	_, totalA, err := svc.List(ctx, tenantA, "", "", nil, nil, 1, 20)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if totalA != 1 {
		t.Fatalf("expected tenant A to see only its own 1 usage log, got %d", totalA)
	}
}

func TestUsageService_Summary_AggregatesRequestAndErrorCounts(t *testing.T) {
	svc := newTestUsageService()
	ctx := context.Background()
	tenantID := "tenant-1"

	_ = svc.Record(ctx, RecordUsageInput{TenantID: &tenantID, Endpoint: "/api/v1/tenants", Method: "GET", StatusCode: 200, LatencyMS: 10, RequestID: "r1"})
	_ = svc.Record(ctx, RecordUsageInput{TenantID: &tenantID, Endpoint: "/api/v1/tenants", Method: "GET", StatusCode: 500, LatencyMS: 30, RequestID: "r2"})

	summary, err := svc.Summary(ctx, tenantID, nil, nil)
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}
	if len(summary) != 1 {
		t.Fatalf("expected 1 aggregated endpoint summary, got %d", len(summary))
	}
	if summary[0].RequestCount != 2 {
		t.Fatalf("expected request_count=2, got %d", summary[0].RequestCount)
	}
	if summary[0].ErrorCount != 1 {
		t.Fatalf("expected error_count=1, got %d", summary[0].ErrorCount)
	}
	if summary[0].AvgLatencyMS != 20 {
		t.Fatalf("expected avg_latency_ms=20, got %v", summary[0].AvgLatencyMS)
	}
}

var _ = strPtr // keep helper available for future tests without unused-import churn
