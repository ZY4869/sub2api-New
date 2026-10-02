package service

// [local] 账号自动续期（本地定制功能）：周期扫描宽限期内的账号，按锚点推进过期时间。
//
// 结构照搬上游 ProxyExpiryService / AccountExpiryService，保持与既有后台服务一致的
// Start/Stop 生命周期语义，便于接入 cmd/server 的启动与优雅关闭流程。

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// ErrAccountRenewalConflict 表示续期期间账号的过期时间被并发改动（或账号已删除）。
// 这不是故障：放弃本次续期，下一轮扫描会基于最新状态重新判定。
var ErrAccountRenewalConflict = errors.New("account renewal: renewal state changed concurrently")

// accountRenewalBatchSize 限制单轮处理的账号数，避免一次扫描长时间占用数据库连接。
const accountRenewalBatchSize = 200

// AccountRenewalCandidate 是一个待续期账号的最小快照。
type AccountRenewalCandidate struct {
	ID                    int64
	Name                  string
	ExpiresAt             time.Time
	Extra                 map[string]any
	RenewalConfigSnapshot json.RawMessage
}

// AccountRenewalRepository 是续期服务所需的窄接口。
//
// 刻意不并入 AccountRepository：那是个被大量测试 stub 实现的宽接口，
// 往里加方法会强制修改一批上游测试文件，徒增合并冲突。
type AccountRenewalRepository interface {
	// ListAccountsPendingRenewal 返回已过期、仍在宽限期内、且有调用正常证据的账号。
	ListAccountsPendingRenewal(ctx context.Context, now time.Time, limit int) ([]AccountRenewalCandidate, error)
	// RenewAccountExpiry 原子推进过期时间并合并续期状态；
	// expectedExpiresAt 用于乐观并发保护，不匹配时返回 ErrAccountRenewalConflict。
	RenewAccountExpiry(
		ctx context.Context,
		id int64,
		newExpiresAt time.Time,
		expectedExpiresAt time.Time,
		expectedConfig json.RawMessage,
		extraUpdates map[string]any,
	) error
}

// AccountRenewalService 周期性地把宽限期内确认可用的账号续期到下一个周期。
type AccountRenewalService struct {
	repo     AccountRenewalRepository
	interval time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewAccountRenewalService 创建续期服务。interval <= 0 时服务不会启动。
func NewAccountRenewalService(repo AccountRenewalRepository, interval time.Duration) *AccountRenewalService {
	return &AccountRenewalService{
		repo:     repo,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

func ProvideAccountRenewalService(repo AccountRenewalRepository) *AccountRenewalService {
	s := NewAccountRenewalService(repo, 5*time.Minute)
	s.Start()
	return s
}

// Start 启动后台扫描循环。
func (s *AccountRenewalService) Start() {
	if s == nil || s.repo == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		s.runOnce()
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
	logger.LegacyPrintf("service.account_renewal", "[AccountRenewal] started (interval=%v)", s.interval)
}

// Stop 停止扫描并等待在途任务结束。
func (s *AccountRenewalService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

func (s *AccountRenewalService) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	now := time.Now()
	candidates, err := s.repo.ListAccountsPendingRenewal(ctx, now, accountRenewalBatchSize)
	if err != nil {
		logger.LegacyPrintf("service.account_renewal", "[AccountRenewal] list candidates failed: %v", err)
		return
	}
	if len(candidates) == 0 {
		return
	}

	renewed := 0
	for _, candidate := range candidates {
		select {
		case <-s.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		if s.renewOne(ctx, candidate, now) {
			renewed++
		}
	}
	if renewed > 0 {
		logger.LegacyPrintf(
			"service.account_renewal",
			"[AccountRenewal] renewed %d/%d accounts", renewed, len(candidates),
		)
	}
}

// renewOne 续期单个账号，返回是否成功。
func (s *AccountRenewalService) renewOne(ctx context.Context, candidate AccountRenewalCandidate, now time.Time) bool {
	cfg := ParseAccountRenewalConfig(candidate.Extra)
	// 仓储在原子写入时校验当前配置；这里仅检查扫描快照。
	if !cfg.Enabled {
		return false
	}

	anchor, cycles := ResolveRenewalBase(candidate.ExpiresAt, cfg, time.Local)
	newExpiresAt, newCycles, ok := ResolveRenewedExpiry(anchor, cfg.Cycle, cycles, now)
	if !ok {
		logger.LegacyPrintf(
			"service.account_renewal",
			"[AccountRenewal] account=%d (%s) skipped: cannot resolve next expiry from anchor=%s cycle=%s",
			candidate.ID, candidate.Name, anchor.Format(time.RFC3339), cfg.Cycle,
		)
		return false
	}

	updates := map[string]any{
		AccountRenewalAnchorExtraKey: anchor.UTC().Format(time.RFC3339),
		AccountRenewalCyclesExtraKey: newCycles,
		AccountRenewalLastAtExtraKey: now.UTC().Format(time.RFC3339),
	}

	if err := s.repo.RenewAccountExpiry(ctx, candidate.ID, newExpiresAt, candidate.ExpiresAt, candidate.RenewalConfigSnapshot, updates); err != nil {
		if errors.Is(err, ErrAccountRenewalConflict) {
			logger.LegacyPrintf(
				"service.account_renewal",
				"[AccountRenewal] account=%d (%s) skipped: renewal state changed concurrently",
				candidate.ID, candidate.Name,
			)
			return false
		}
		logger.LegacyPrintf(
			"service.account_renewal",
			"[AccountRenewal] account=%d (%s) renew failed: %v", candidate.ID, candidate.Name, err,
		)
		return false
	}

	logger.LegacyPrintf(
		"service.account_renewal",
		"[AccountRenewal] account=%d (%s) renewed: %s -> %s (cycle=%s, total_cycles=%d)",
		candidate.ID, candidate.Name,
		candidate.ExpiresAt.Format(time.RFC3339), newExpiresAt.Format(time.RFC3339),
		cfg.Cycle, newCycles,
	)
	return true
}
