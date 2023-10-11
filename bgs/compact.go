package bgs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/carstore"
	"github.com/bluesky-social/indigo/models"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type queueItem struct {
	uid  models.Uid
	fast bool
}

type queue struct {
	q       []queueItem
	members map[models.Uid]struct{}
	lk      sync.Mutex
}

func (q *queue) Append(uid models.Uid, fast bool) {
	q.lk.Lock()
	defer q.lk.Unlock()

	if _, ok := q.members[uid]; ok {
		return
	}

	q.q = append(q.q, queueItem{uid: uid, fast: fast})
	q.members[uid] = struct{}{}
}

func (q *queue) Prepend(uid models.Uid, fast bool) {
	q.lk.Lock()
	defer q.lk.Unlock()

	if _, ok := q.members[uid]; ok {
		return
	}

	q.q = append([]queueItem{{uid: uid, fast: fast}}, q.q...)
	q.members[uid] = struct{}{}
}

func (q *queue) Has(uid models.Uid) bool {
	q.lk.Lock()
	defer q.lk.Unlock()

	_, ok := q.members[uid]
	return ok
}

func (q *queue) Remove(uid models.Uid) {
	q.lk.Lock()
	defer q.lk.Unlock()

	if _, ok := q.members[uid]; !ok {
		return
	}

	for i, item := range q.q {
		if item.uid == uid {
			q.q = append(q.q[:i], q.q[i+1:]...)
			break
		}
	}

	delete(q.members, uid)
}

func (q *queue) Pop() (*queueItem, bool) {
	q.lk.Lock()
	defer q.lk.Unlock()

	if len(q.q) == 0 {
		return nil, false
	}

	item := q.q[0]
	q.q = q.q[1:]
	delete(q.members, item.uid)

	return &item, true
}

type CompactorState struct {
	latestUID models.Uid
	latestDID string
	status    string
	stats     *carstore.CompactionStats
}

type Compactor struct {
	q       *queue
	state   *CompactorState
	stateLk sync.RWMutex
	exit    chan struct{}
}

func NewCompactor() *Compactor {
	return &Compactor{
		q: &queue{
			members: make(map[models.Uid]struct{}),
		},
	}
}

type compactionStats struct {
	Completed map[models.Uid]*carstore.CompactionStats
	Targets   []carstore.CompactionTarget
}

func (c *Compactor) SetState(uid models.Uid, did, status string, stats *carstore.CompactionStats) {
	c.stateLk.Lock()
	defer c.stateLk.Unlock()

	c.state.latestUID = uid
	c.state.latestDID = did
	c.state.status = status
	c.state.stats = stats
}

func (c *Compactor) GetState() *CompactorState {
	c.stateLk.RLock()
	defer c.stateLk.RUnlock()

	return &CompactorState{
		latestUID: c.state.latestUID,
		latestDID: c.state.latestDID,
		status:    c.state.status,
		stats:     c.state.stats,
	}
}

var errNoReposToCompact = fmt.Errorf("no repos to compact")

func (c *Compactor) Run(bgs *BGS) {
	for {
		select {
		case <-c.exit:
			log.Warn("compactor exiting")
			return
		default:
		}

		ctx := context.Background()
		start := time.Now()
		state, err := c.CompactNext(ctx, bgs)
		if err != nil {
			if err == errNoReposToCompact {
				log.Warn("no repos to compact, waiting and retrying")
				time.Sleep(time.Second * 5)
				continue
			}
			log.Errorw("failed to compact repo",
				"err", err,
				"uid", state.latestUID,
				"repo", state.latestDID,
				"status", state.status,
				"stats", state.stats,
				"duration", time.Since(start),
			)
			// Pause for a bit to avoid spamming failed compactions
			time.Sleep(time.Millisecond * 100)
		} else {
			log.Warnw("compacted repo",
				"uid", state.latestUID,
				"repo", state.latestDID,
				"status", state.status,
				"stats", state.stats,
				"duration", time.Since(start),
			)
		}
	}
}

func (c *Compactor) CompactNext(ctx context.Context, bgs *BGS) (*CompactorState, error) {
	ctx, span := otel.Tracer("bgs").Start(ctx, "CompactNext")
	defer span.End()

	item, ok := c.q.Pop()
	if !ok || item == nil {
		return nil, errNoReposToCompact
	}

	c.SetState(item.uid, "unknown", "getting_user", nil)

	user, err := bgs.lookupUserByUID(ctx, item.uid)
	if err != nil {
		c.SetState(item.uid, "unknown", "failed_getting_user", nil)
		return nil, fmt.Errorf("failed to get user %d: %w", item.uid, err)
	}

	c.SetState(item.uid, user.Did, "compacting", nil)

	start := time.Now()
	st, err := bgs.repoman.CarStore().CompactUserShards(ctx, item.uid, item.fast)
	if err != nil {
		c.SetState(item.uid, user.Did, "failed_compacting", nil)
		return nil, fmt.Errorf("failed to compact shards for user %d: %w", item.uid, err)
	}
	compactionDuration.Observe(time.Since(start).Seconds())

	c.SetState(item.uid, user.Did, "done", st)

	return c.GetState(), nil
}

func (c *Compactor) EnqueueRepo(ctx context.Context, user User, fast bool) {
	log.Infow("enqueueing compaction for repo", "repo", user.Did, "uid", user.ID, "fast", fast)
	c.q.Append(user.ID, fast)
}

// EnqueueAllRepos enqueues all repos for compaction
// lim is the maximum number of repos to enqueue
// shardCount is the number of shards to compact per user (0 = default of 50)
// fast is whether to use the fast compaction method (skip large shards)
func (c *Compactor) EnqueueAllRepos(ctx context.Context, bgs *BGS, lim int, shardCount int, fast bool) error {
	ctx, span := otel.Tracer("bgs").Start(ctx, "EnqueueAllRepos")
	defer span.End()

	span.SetAttributes(
		attribute.Int("lim", lim),
		attribute.Int("shardCount", shardCount),
		attribute.Bool("fast", fast),
	)

	if shardCount == 0 {
		shardCount = 50
	}

	span.SetAttributes(attribute.Int("clampedShardCount", shardCount))

	log := log.With("source", "compactor_enqueue_all_repos", "lim", lim, "shardCount", shardCount, "fast", fast)
	log.Warn("enqueueing all repos")

	repos, err := bgs.repoman.CarStore().GetCompactionTargets(ctx, shardCount)
	if err != nil {
		return fmt.Errorf("failed to get repos to compact: %w", err)
	}

	span.SetAttributes(attribute.Int("repos", len(repos)))

	if lim > 0 && len(repos) > lim {
		repos = repos[:lim]
	}

	span.SetAttributes(attribute.Int("clampedRepos", len(repos)))

	for _, r := range repos {
		c.q.Append(r.Usr, fast)
	}

	log.Warn("done enqueueing all repos")

	return nil
}

func (bgs *BGS) runRepoCompaction(ctx context.Context, lim int, dry bool, fast bool) (*compactionStats, error) {
	ctx, span := otel.Tracer("bgs").Start(ctx, "runRepoCompaction")
	defer span.End()

	log.Warn("starting repo compaction")

	runStart := time.Now()

	repos, err := bgs.repoman.CarStore().GetCompactionTargets(ctx, 50)
	if err != nil {
		return nil, fmt.Errorf("failed to get repos to compact: %w", err)
	}

	if lim > 0 && len(repos) > lim {
		repos = repos[:lim]
	}

	if dry {
		return &compactionStats{
			Targets: repos,
		}, nil
	}

	results := make(map[models.Uid]*carstore.CompactionStats)
	for i, r := range repos {
		select {
		case <-ctx.Done():
			return &compactionStats{
				Targets:   repos,
				Completed: results,
			}, nil
		default:
		}

		repostart := time.Now()
		st, err := bgs.repoman.CarStore().CompactUserShards(context.Background(), r.Usr, fast)
		if err != nil {
			log.Errorf("failed to compact shards for user %d: %s", r.Usr, err)
			continue
		}
		compactionDuration.Observe(time.Since(repostart).Seconds())
		results[r.Usr] = st

		if i%100 == 0 {
			log.Warnf("compacted %d repos in %s", i+1, time.Since(runStart))
		}
	}

	log.Warnf("compacted %d repos in %s", len(repos), time.Since(runStart))

	return &compactionStats{
		Targets:   repos,
		Completed: results,
	}, nil
}
