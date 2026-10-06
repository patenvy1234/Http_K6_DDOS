// Package runner implements the full lifecycle of a `ddos run`:
//
//  1. Validate profile
//  2. Create per-run resource group
//  3. Deploy foundation.bicep → UAMI + storage + roles
//  4. Render cloud-init (embeds storage account name)
//  5. Generate per-VM profile.json and build bundle.tar.gz
//  6. Upload bundle to blob via OAuth (--auth-mode login; operator gets Storage
//     Blob Data Contributor); workers download via UAMI
//  7. Deploy fleet.bicep → VMSS per region (custom data already rendered)
//  8. Watch loop: spot-eviction refill + completion detection
//  9. Cleanup on signal, error, or natural completion
package runner

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/azcli"
	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/bicepdeploy"
	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/quotacache"
	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/skucache"
	"github.com/patenvy1234/Http_K6_DDOS/internal/config"
	"github.com/patenvy1234/Http_K6_DDOS/internal/config/userconfig"
	"github.com/patenvy1234/Http_K6_DDOS/internal/monitor"
	"github.com/patenvy1234/Http_K6_DDOS/internal/orchestrator/bundle"
	"github.com/patenvy1234/Http_K6_DDOS/internal/orchestrator/cloudinit"
	"github.com/patenvy1234/Http_K6_DDOS/internal/orchestrator/cost"
	"github.com/patenvy1234/Http_K6_DDOS/internal/orchestrator/profilegen"
	"github.com/patenvy1234/Http_K6_DDOS/internal/orchestrator/runspec"
)

// Options are optional flags from the CLI layer.
type Options struct {
	MaxCostUSD float64 // refuse to deploy if estimated cost exceeds this; zero disables the guard
}

// Run executes one complete load run and blocks until the test finishes or the
// context is cancelled. Cleanup is best-effort and always attempted.
func Run(ctx context.Context, profilePath string, uc *userconfig.File, opts Options) error {
	// --- 1. Validate profile ---
	profile, err := config.LoadAndValidate(profilePath)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	estimate, err := cost.Enforce(profile, opts.MaxCostUSD)
	if err != nil {
		return fmt.Errorf("run: cost guard: %w", err)
	}

	// Get the current Azure user as the owner tag.
	az := azcli.New()
	account, err := az.CurrentAccount(ctx)
	if err != nil {
		return fmt.Errorf("run: not logged in: %w", err)
	}
	if uc.SubscriptionID != "" {
		if err := az.SetSubscription(ctx, uc.SubscriptionID); err != nil {
			return fmt.Errorf("run: set subscription: %w", err)
		}
	}

	// --- 2. Derive RunSpec ---
	spec, err := runspec.New(profile, account.User.Name)
	if err != nil {
		return fmt.Errorf("run: runspec: %w", err)
	}

	logf("run %s (suffix=%s) starting", spec.RunName, spec.RunSuffix)
	if dir, lerr := openRunLog(spec.RunName, spec.RunSuffix); lerr == nil {
		snapshotProfile(profilePath, dir)
		logf("log file: %s", filepath.Join(dir, "run.log"))
		defer closeRunLog()
	} else {
		logf("⚠  could not open run log file: %v", lerr)
	}
	logf("resource group: %s", spec.ResourceGroup)
	logf("regions: %s", strings.Join(profile.Fleet.Regions, ", "))
	if opts.MaxCostUSD > 0 {
		logf("cost guard: planning estimate $%.2f / $%.2f limit", estimate.TotalUSD, opts.MaxCostUSD)
	} else {
		logf("⚠  cost guard disabled; planning estimate $%.2f", estimate.TotalUSD)
	}

	// Install signal handler so Ctrl-C triggers cleanup.
	sigCtx, stopSignal := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignal()

	// Cleanup is always attempted, even on error.
	rgCreated := false
	defer func() {
		if rgCreated && profile.Lifecycle.CleanupOnInterrupt {
			logf("cleaning up resource group %s ...", spec.ResourceGroup)
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancelCleanup()
			if cerr := az.DeleteResourceGroup(cleanupCtx, spec.ResourceGroup); cerr != nil {
				logf("⚠  cleanup failed (you may need to delete manually): %v", cerr)
				logf("   az group delete --name %s --yes --no-wait", spec.ResourceGroup)
			} else {
				logf("✓ delete initiated for %s", spec.ResourceGroup)
			}
		}
	}()

	// --- 3. Load quota/SKU cache and resolve available regions ---
	// Region resolution MUST run before RG creation: with `regions: []`
	// (auto-expand), Fleet.Regions is empty until resolved here, and RG
	// creation below indexes Regions[0] for the resource-group location.
	cache, cacheErr := skucache.Load("")
	qcache, _ := quotacache.Load(quotaFilePath())

	if qcache != nil {
		logf("preflight: resolving %s from quota cache (generated %s) ...",
			profile.Fleet.VMSize, qcache.GeneratedAt.Format(time.RFC3339))
	} else if cacheErr != nil {
		logf("⚠  no quota cache — run `ddos refresh-quota` for quota-aware region selection")
	} else {
		logf("⚠  no quota cache — falling back to SKU cache; run `ddos refresh-quota` for better region selection")
	}

	if qcache != nil || cache != nil {
		resolved, rerr := resolveRegions(qcache, cache, profile.Fleet.VMSize, profile.Fleet.Regions, nil)
		if rerr != nil {
			return fmt.Errorf("run: region preflight: %w", rerr)
		}
		if !equalStringSlice(resolved, profile.Fleet.Regions) {
			logf("✓ regions resolved: %s (was: %s)",
				strings.Join(resolved, ", "), strings.Join(profile.Fleet.Regions, ", "))
		} else {
			logf("✓ all requested regions available: %s", strings.Join(resolved, ", "))
		}
		profile.Fleet.Regions = resolved
	}
	if len(profile.Fleet.Regions) == 0 {
		return fmt.Errorf("run: no regions resolved for %s — set fleet.regions explicitly or run `ddos refresh-quota`", profile.Fleet.VMSize)
	}
	vmPerRegion, regionCount, err := profilegen.PlanInitialFleet(profile)
	if err != nil {
		return fmt.Errorf("run: fleet sizing: %w", err)
	}
	profile.Fleet.Regions = profile.Fleet.Regions[:regionCount]
	profile.Fleet.InitialVMCount = vmPerRegion
	logf("fleet sizing: %d total VMs (%d/region across %s), each targeting %d RPS",
		vmPerRegion*regionCount, vmPerRegion, strings.Join(profile.Fleet.Regions, ", "), profilegen.FixedPerVMRPS)

	// --- 4. Create resource group ---
	logf("creating resource group %s in %s ...", spec.ResourceGroup, profile.Fleet.Regions[0])
	if err := az.CreateResourceGroup(sigCtx, spec.ResourceGroup, profile.Fleet.Regions[0]); err != nil {
		return fmt.Errorf("run: create RG: %w", err)
	}
	rgCreated = true

	// --- 5. Deploy foundation (UAMI + storage) ---
	logf("deploying foundation (UAMI + storage + roles) ...")
	bicepDir := bicepDirectory()
	deployerObjectID, err := az.GetSignedInUserObjectID(sigCtx)
	if err != nil {
		return fmt.Errorf("run: get signed-in user: %w", err)
	}
	foundationOuts, err := bicepdeploy.ResourceGroupScope(sigCtx, az, spec.ResourceGroup, bicepdeploy.Options{
		Name:         "ddos-foundation-" + spec.RunSuffix,
		TemplateFile: filepath.Join(bicepDir, "foundation.bicep"),
		Parameters:   foundationParams(profile, spec, uc, deployerObjectID),
	})
	if err != nil {
		return fmt.Errorf("run: foundation deploy: %w", err)
	}
	storageAccount := foundationOuts.String("storageAccountName")
	blobContainer := foundationOuts.String("blobContainerName")
	uamiResourceID := foundationOuts.String("uamiResourceId")
	uamiClientID := foundationOuts.String("uamiClientId")
	logf("✓ foundation deployed (storage=%s)", storageAccount)

	// --- 6. Render cloud-init (now storage account name is known) ---

	// Validate observability config early before rendering.
	if _, err := buildRemoteWriteURL(uc); err != nil {
		return fmt.Errorf("run: remote write URL: %w", err)
	}
	customData, err := cloudinit.RenderBase64(cloudinit.Input{
		RunID:          spec.RunSuffix,
		RunName:        spec.RunName,
		AdminUsername:  "ddos",
		SSHPublicKey:   strings.TrimSpace(spec.SSHPublicKeyOpen),
		DCEURL:         uc.Observability.DCEURL,
		DCRImmutableID: uc.Observability.DCRImmutableID,
		DCRStreamName:  "Microsoft-PrometheusMetrics",
		StorageAccount: storageAccount,
		BlobContainer:  blobContainer,
		BundleBlob:     "bundle.tar.gz",
		K6Version:      "0.58.0",
	})
	if err != nil {
		return fmt.Errorf("run: cloud-init render: %w", err)
	}

	// --- 7. Generate profile.json + bundle ---
	// Each VM targets FixedPerVMRPS; fleet size is derived after region resolution.
	profileJSON, err := profilegen.Generate(profile, spec)
	if err != nil {
		return fmt.Errorf("run: profilegen: %w", err)
	}
	bundleBytes, err := bundle.Build(profileJSON)
	if err != nil {
		return fmt.Errorf("run: bundle build: %w", err)
	}
	logf("bundle size: %d KB", len(bundleBytes)/1024)

	// --- 8. Upload bundle (OAuth/AAD; operator was granted Storage Blob Data
	// Contributor on the storage account by foundation.bicep) ---
	logf("uploading bundle.tar.gz to %s/%s ...", storageAccount, blobContainer)
	if err := az.BlobUpload(sigCtx, storageAccount, blobContainer, "bundle.tar.gz", bundleBytes); err != nil {
		return fmt.Errorf("run: blob upload: %w", err)
	}
	logf("✓ bundle uploaded")

	excludedRegions := map[string]bool{}
	logf("deploying fleet (%d VMs x %d regions: %s) ...", profile.Fleet.InitialVMCount,
		len(profile.Fleet.Regions), strings.Join(profile.Fleet.Regions, ", "))
	fleetOuts, err := bicepdeploy.ResourceGroupScope(sigCtx, az, spec.ResourceGroup, bicepdeploy.Options{
		Name:         "ddos-fleet-" + spec.RunSuffix,
		TemplateFile: filepath.Join(bicepDir, "fleet.bicep"),
		Parameters:   fleetParams(profile, spec, customData, uamiResourceID, uamiClientID),
	})
	if err != nil {
		return fmt.Errorf("run: fleet deploy failed; aborting rather than duplicating a partially deployed fleet: %w", err)
	}
	vmssNames := fleetOuts.StringArray("vmssNames")
	if len(vmssNames) != len(profile.Fleet.Regions) {
		return fmt.Errorf("run: fleet deploy returned %d VMSS names for %d regions", len(vmssNames), len(profile.Fleet.Regions))
	}
	logf("✓ fleet deployed: %s", strings.Join(vmssNames, ", "))
	logf("start at: %s", spec.StartAt.Format(time.RFC3339))
	logf("opening Grafana: %s", uc.Observability.GrafanaURL)

	// Build the per-VMSS → region mapping that the scaler needs. fleet.bicep
	// emits vmssNames in the same order as the `regions` slice it was given.
	initialRegions := append([]string(nil), profile.Fleet.Regions...)

	// --- 9. Watch loop ---
	amwClient := monitor.New(uc.Observability.PrometheusQueryURL, az)
	return watchLoop(sigCtx, az, amwClient, profile, spec, spec.ResourceGroup, vmssNames, initialRegions,
		uc, cache, qcache, bicepDir, customData, uamiResourceID, uamiClientID, excludedRegions)
}

// watchState holds mutable scaler state across poll ticks.
//
// `capByVMSS` is the source of truth for each VMSS's current target capacity
// (the scaler reads/updates it; `refillEvictions` uses it to detect spot
// loss). `regionByVMSS` lets us map each VMSS back to its region, so we know
// which regions are already in use and avoid double-deploying.
type watchState struct {
	// mu guards every field below.
	//
	// scaleFleet and the per-tick refill goroutine acquire it for brief
	// critical sections (snapshot reads, counter writes, map updates) and
	// release it around long-running `az` calls so the ticker can continue
	// observing the deadline regardless of how slow an ARM call is.
	mu sync.Mutex

	// scaling/refilling are TRUE while a goroutine started by the ticker is
	// in flight, so the next tick can skip launching a duplicate.
	scaling   bool
	refilling bool

	vmssNames          []string       // ordered (initial + scaler-added)
	capByVMSS          map[string]int // vmssName → current target capacity
	uncertainCapacity  int
	regionByVMSS       map[string]string // vmssName → region
	usedRegions        map[string]bool   // region → true if a VMSS lives there
	excludedRegions    map[string]bool   // region → true if known capacity-exhausted
	belowTargetTicks   int               // consecutive polls where observed < 95% target
	saturationTicks    int               // consecutive polls where dropped_iter > threshold (and below target)
	scaleCooldownUntil time.Time         // don't scale again until this time
	lastObservedRPS    float64
}

// totalCapacityLocked sums the per-VMSS target capacities.
// Caller must hold st.mu.
func (st *watchState) totalCapacityLocked() int {
	total := st.uncertainCapacity
	for _, c := range st.capByVMSS {
		total += c
	}
	return total
}

// snapshotFleet returns a copy of vmssNames + capByVMSS taken under lock.
// Used by the ticker so the eviction-refill goroutine can iterate without
// holding st.mu for the duration of `az vmss list-instances` calls.
func (st *watchState) snapshotFleet() ([]string, map[string]int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	names := append([]string(nil), st.vmssNames...)
	caps := make(map[string]int, len(st.capByVMSS))
	for k, v := range st.capByVMSS {
		caps[k] = v
	}
	return names, caps
}

// tryStart{Scale,Refill} atomically check-and-set the corresponding flag.
// Returns false if a previous goroutine of that kind is still running.
func (st *watchState) tryStartScale() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.scaling {
		return false
	}
	st.scaling = true
	return true
}

func (st *watchState) endScale() {
	st.mu.Lock()
	st.scaling = false
	st.mu.Unlock()
}

func (st *watchState) tryStartRefill() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.refilling {
		return false
	}
	st.refilling = true
	return true
}

func (st *watchState) endRefill() {
	st.mu.Lock()
	st.refilling = false
	st.mu.Unlock()
}

// inCooldown reports whether scaling is currently suppressed because a recent
// scale operation completed within the cooldown window.
func (st *watchState) inCooldown(now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return now.Before(st.scaleCooldownUntil)
}

// watchLoop blocks until the test completes naturally or the context is cancelled.
// Every 30 s it:
//   - refills spot evictions
//   - queries AMW for actual RPS (after warm-up)
//   - if observed RPS is below target OR k6 is saturated, scales out by
//     PREFERRING a fresh region (better SNAT, better spot availability) and
//     only falling back to scaling existing VMSS once all candidate regions
//     are used up.
func watchLoop(
	ctx context.Context,
	az *azcli.Runner,
	amw *monitor.Client,
	profile *config.AttackProfile,
	spec *runspec.RunSpec,
	rg string,
	vmssNames []string,
	regions []string,
	uc *userconfig.File,
	cache *skucache.File,
	qcache *quotacache.File,
	bicepDir string,
	customData string,
	uamiResourceID string,
	uamiClientID string,
	excludedRegions map[string]bool,
) error {
	totalSec := profile.Target.RampUpSeconds + profile.Target.DurationSeconds + profile.Target.RampDownSeconds
	deadline := spec.StartAt.Add(time.Duration(totalSec)*time.Second + 3*time.Minute)
	watchCtx, cancelWatch := context.WithDeadline(ctx, deadline)
	var operations sync.WaitGroup
	defer func() {
		cancelWatch()
		operations.Wait()
	}()
	// Warm-up: don't query AMW until workers have been generating traffic
	// AND Prometheus remote_write has had time to ingest several scrapes.
	// 120s post-ramp lets k6 reach steady state and AMW catch up before any
	// scale decision is considered.
	warmupDone := spec.StartAt.Add(time.Duration(profile.Target.RampUpSeconds)*time.Second + 120*time.Second)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	logf("monitoring (deadline %s, Grafana %s) ...", deadline.Format(time.RFC3339), uc.Observability.GrafanaURL)

	st := &watchState{
		vmssNames:       append([]string(nil), vmssNames...),
		capByVMSS:       make(map[string]int, len(vmssNames)),
		regionByVMSS:    make(map[string]string, len(vmssNames)),
		usedRegions:     make(map[string]bool, len(regions)),
		excludedRegions: map[string]bool{},
	}
	if excludedRegions != nil {
		for r := range excludedRegions {
			st.excludedRegions[r] = true
		}
	}
	for i, name := range vmssNames {
		st.capByVMSS[name] = profile.Fleet.InitialVMCount
		if i < len(regions) {
			st.regionByVMSS[name] = regions[i]
			st.usedRegions[regions[i]] = true
		}
	}

	for {
		select {
		case <-watchCtx.Done():
			if err := ctx.Err(); err != nil {
				logf("interrupted")
				return err
			}
			logf("test duration elapsed, initiating cleanup")
			return nil

		case <-ticker.C:
			// Use time.Now() rather than the ticker's scheduled-fire time so
			// the deadline is checked against wall-clock time even when a
			// previous tick handler queued up while the loop was busy.
			now := time.Now()
			if now.After(deadline) {
				logf("✓ test duration elapsed, initiating cleanup")
				return nil
			}

			// Spot eviction refill — runs in a background goroutine so a
			// hung `az vmss list-instances` (during eviction storms or ARM
			// throttling) does NOT block the watchloop from observing the
			// deadline. tryStartRefill prevents pile-ups: if the previous
			// refill goroutine is still in flight we simply skip this tick.
			if st.tryStartRefill() {
				snapshotNames, snapshotCaps := st.snapshotFleet()
				operations.Add(1)
				go func() {
					defer operations.Done()
					defer st.endRefill()
					for _, vmssName := range snapshotNames {
						if watchCtx.Err() != nil {
							return
						}
						refillCtx, refillCancel := context.WithTimeout(watchCtx, 25*time.Second)
						err := refillEvictions(refillCtx, az, rg, vmssName, snapshotCaps[vmssName], perVMSSHardCap)
						refillCancel()
						if err != nil {
							logf("⚠  eviction refill %s: %v", vmssName, err)
						}
					}
				}()
			}

			// AMW-based RPS autoscaler (only after warm-up and cooldown).
			// scaleFleet runs in a background goroutine: a Bicep region
			// deploy can take 3-6 minutes (longer when az.exe is wedged on
			// Windows because the orphaned python process keeps stdio open),
			// and we MUST not let that block the ticker from firing the
			// deadline check. tryStartScale prevents two scale ops from
			// overlapping if a previous one is still running.
			if now.After(warmupDone) && !st.inCooldown(now) && uc.Observability.PrometheusQueryURL != "" {
				if st.tryStartScale() {
					operations.Add(1)
					go func() {
						defer operations.Done()
						defer st.endScale()
						scaleFleet(watchCtx, az, amw, profile, spec, rg, st, cache, qcache, bicepDir, customData, uamiResourceID, uamiClientID)
					}()
				}
			}
		}
	}
}

// droppedSaturationThreshold is the iterations/s above which we consider a
// worker fleet saturated. k6 only drops iterations when its arrival-rate
// scheduler cannot find a free VU in time — a direct signal that the VMs
// are CPU-bound, conntrack-bound, or port-exhausted.
const droppedSaturationThreshold = 5.0

// perVMSSHardCap is the per-VMSS instance ceiling enforced by Bicep
// (modules/region.bicep has @maxValue(200) on capacity).
const perVMSSHardCap = profilegen.MaxVMSSCapacity

// scaleFleet queries AMW for actual RPS *and* dropped-iteration rate, then
// decides how to add capacity. The growth strategy is:
//
//  1. PREFER a fresh region: if any candidate region in the SKU cache is not
//     yet used (and not on the exclusion list), deploy a brand-new VMSS
//     there. Different region → independent SNAT pool, independent spot
//     capacity, fresh per-VMSS budget — beats scaling an existing VMSS in
//     almost every dimension.
//  2. Only when ALL eligible candidate regions are exhausted, fall back to
//     uniformly scaling existing VMSS up to perVMSSHardCap.
//
// Scaling is triggered ONLY when aggregate RPS is below 95% of target for
// ≥2 consecutive 30s polls. If user asked for 5K and we observe 5.3K, we
// don't scale — k6's per-VM 5,000 aspiration is a ceiling, not a goal.
//
// Saturation (dropped_iter > threshold) by itself is NOT a scale trigger:
// k6's constant-arrival-rate scheduler always drops iterations when the
// per-VM 5,000 RPS aspiration exceeds what one VM can deliver — even when
// the *user's* target is comfortably met. We only honour saturation when it
// coincides with under-target throughput (≥2 consecutive ticks), at which
// point the deficit math already covers it.
func scaleFleet(
	ctx context.Context,
	az *azcli.Runner,
	amw *monitor.Client,
	profile *config.AttackProfile,
	spec *runspec.RunSpec,
	rg string,
	st *watchState,
	cache *skucache.File,
	qcache *quotacache.File,
	bicepDir string,
	customData string,
	uamiResourceID string,
	uamiClientID string,
) {
	observed, err := amw.ObservedRPS(ctx, spec.RunSuffix)
	if err != nil {
		logf("⚠  AMW query: %v", err)
		return
	}
	dropped, derr := amw.ObservedDroppedRate(ctx, spec.RunSuffix)
	if derr != nil {
		logf("⚠  AMW dropped query: %v", derr)
		// continue with deficit-only logic
	}
	saturated := dropped > droppedSaturationThreshold

	target := float64(profile.Target.RPS)
	pct := 0.0
	if target > 0 {
		pct = observed / target * 100
	}
	if dropped > 0 {
		logf("observed %.0f RPS / target %.0f RPS (%.1f%%) — k6 dropping %.0f iter/s", observed, target, pct, dropped)
	} else {
		logf("observed %.0f RPS / target %.0f RPS (%.1f%%)", observed, target, pct)
	}

	below := observed < target*0.95

	// --- Counter update + decision-to-scale (under lock) ---
	st.mu.Lock()
	st.lastObservedRPS = observed
	if below {
		st.belowTargetTicks++
	} else {
		st.belowTargetTicks = 0
	}
	if saturated && below {
		st.saturationTicks++
	} else {
		st.saturationTicks = 0
	}

	// Scale ONLY when we are below user target for 2 consecutive 30s polls.
	// Saturation alone (dropped iter while ≥95% target) is expected: k6 is
	// always aspirationally driving 5,000 RPS/VM regardless of user target.
	if !below || st.belowTargetTicks < 2 {
		st.mu.Unlock()
		return
	}
	st.belowTargetTicks = 0
	st.saturationTicks = 0

	totalVMs := st.totalCapacityLocked()
	if totalVMs == 0 {
		st.mu.Unlock()
		return
	}

	deficit := target - observed
	if deficit < 0 {
		deficit = 0
	}
	var extraTotal int
	if observed > 0 {
		perVMRPS := observed / float64(totalVMs)
		if perVMRPS < 1 {
			perVMRPS = 1
		}
		extraTotal = int(math.Ceil(deficit / perVMRPS))
	}
	// Saturation co-incident with below-target: bump extras by ~25% so each
	// existing worker has headroom (otherwise deficit math may undershoot
	// when per-VM RPS is itself depressed by drops).
	if saturated {
		minExtra := (totalVMs + 3) / 4
		if minExtra < 1 {
			minExtra = 1
		}
		if extraTotal < minExtra {
			extraTotal = minExtra
		}
	}
	if extraTotal <= 0 {
		st.mu.Unlock()
		return
	}

	// Total-VM budget: never exceed MaxVMCount across the whole fleet.
	remainingBudget := profile.Fleet.MaxVMCount - totalVMs
	if remainingBudget <= 0 {
		st.mu.Unlock()
		logf("⚠  fleet at MaxVMCount (%d VMs); cannot scale further", profile.Fleet.MaxVMCount)
		return
	}
	if extraTotal > remainingBudget {
		logf("ℹ  deficit asks for %d VMs but only %d remain in MaxVMCount budget; capping", extraTotal, remainingBudget)
		extraTotal = remainingBudget
	}

	// Snapshot the maps PlanAllocation/pickFreshRegion need so we can drop
	// the lock during the long Bicep deploys that follow.
	usedSnap := make(map[string]bool, len(st.usedRegions))
	for k, v := range st.usedRegions {
		usedSnap[k] = v
	}
	excludedSnap := make(map[string]bool, len(st.excludedRegions))
	for k, v := range st.excludedRegions {
		excludedSnap[k] = v
	}
	st.mu.Unlock()

	reason := fmt.Sprintf("close %.0f RPS deficit", deficit)
	if saturated {
		reason = fmt.Sprintf("close %.0f RPS deficit + relieve %.0f dropped iter/s", deficit, dropped)
	}

	// --- Phase 1: prefer fresh regions (lock released during az calls) ---
	addedFreshRegions := 0
	if qcache != nil {
		// Quota-aware round-robin: PlanAllocation returns the best spread.
		allocs := qcache.PlanAllocation(profile.Fleet.VMSize, extraTotal, usedSnap, excludedSnap)
		for _, alloc := range allocs {
			if ctx.Err() != nil {
				return
			}
			if extraTotal <= 0 {
				break
			}
			newCap := alloc.Count
			if newCap > perVMSSHardCap {
				newCap = perVMSSHardCap
			}
			logf("scaling out to fresh region %s (capacity=%d) to %s [quota-aware]", alloc.Region, newCap, reason)
			vmssName, addErr := addRegionVMSS(ctx, az, profile, spec, rg, alloc.Region, newCap,
				bicepDir, customData, uamiResourceID, uamiClientID)
			st.mu.Lock()
			if addErr != nil {
				st.uncertainCapacity += newCap
				extraTotal -= newCap
				logf("reserving %d VM slots for uncertain deployment in %s until cleanup", newCap, alloc.Region)
				if isCapacityError(addErr) {
					logf("⚠  fresh region %s has no capacity; excluding", alloc.Region)
				} else {
					logf("⚠  fresh region %s deploy failed: %v", alloc.Region, addErr)
				}
				st.excludedRegions[alloc.Region] = true
				st.mu.Unlock()
				continue
			}
			st.vmssNames = append(st.vmssNames, vmssName)
			st.capByVMSS[vmssName] = newCap
			st.regionByVMSS[vmssName] = alloc.Region
			st.usedRegions[alloc.Region] = true
			st.mu.Unlock()
			addedFreshRegions++
			extraTotal -= newCap
		}
	} else {
		for extraTotal > 0 && cache != nil {
			if ctx.Err() != nil {
				return
			}
			st.mu.Lock()
			region := pickFreshRegion(cache, profile.Fleet.VMSize, st.usedRegions, st.excludedRegions)
			st.mu.Unlock()
			if region == "" {
				break // no more fresh regions available
			}
			newCap := profile.Fleet.InitialVMCount
			if newCap > extraTotal {
				newCap = extraTotal
			}
			if newCap > perVMSSHardCap {
				newCap = perVMSSHardCap
			}
			logf("scaling out to fresh region %s (capacity=%d) to %s", region, newCap, reason)
			vmssName, addErr := addRegionVMSS(ctx, az, profile, spec, rg, region, newCap,
				bicepDir, customData, uamiResourceID, uamiClientID)
			st.mu.Lock()
			if addErr != nil {
				st.uncertainCapacity += newCap
				extraTotal -= newCap
				logf("reserving %d VM slots for uncertain deployment in %s until cleanup", newCap, region)
				if isCapacityError(addErr) {
					logf("⚠  fresh region %s has no capacity; excluding and trying another", region)
				} else {
					logf("⚠  fresh region %s deploy failed: %v", region, addErr)
				}
				st.excludedRegions[region] = true
				st.mu.Unlock()
				continue
			}
			st.vmssNames = append(st.vmssNames, vmssName)
			st.capByVMSS[vmssName] = newCap
			st.regionByVMSS[vmssName] = region
			st.usedRegions[region] = true
			st.mu.Unlock()
			addedFreshRegions++
			extraTotal -= newCap
		}
	}

	// --- Phase 2: fall back to scaling existing VMSS ---
	if extraTotal > 0 {
		st.mu.Lock()
		fleetSnap := append([]string(nil), st.vmssNames...)
		capSnap := make(map[string]int, len(st.capByVMSS))
		for k, v := range st.capByVMSS {
			capSnap[k] = v
		}
		regionSnap := make(map[string]string, len(st.regionByVMSS))
		for k, v := range st.regionByVMSS {
			regionSnap[k] = v
		}
		st.mu.Unlock()

		if len(fleetSnap) > 0 {
			nVMSS := len(fleetSnap)
			extraPerVMSS := (extraTotal + nVMSS - 1) / nVMSS // ceil div
			grew := false
			for _, vmssName := range fleetSnap {
				if ctx.Err() != nil {
					return
				}
				if extraTotal <= 0 {
					break
				}
				curCap := capSnap[vmssName]
				newCap := nextVMSSCapacity(curCap, extraPerVMSS, extraTotal)
				if newCap <= curCap {
					continue
				}
				logf("scaling existing VMSS %s: %d → %d (region %s, %s)",
					vmssName, curCap, newCap, regionSnap[vmssName], reason)
				if err := az.VMSSScale(ctx, rg, vmssName, newCap); err != nil {
					logf("⚠  scale %s: %v", vmssName, err)
					st.mu.Lock()
					st.uncertainCapacity += newCap - curCap
					st.mu.Unlock()
					extraTotal -= newCap - curCap
					continue
				}
				st.mu.Lock()
				st.capByVMSS[vmssName] = newCap
				st.mu.Unlock()
				extraTotal -= newCap - curCap
				grew = true
			}
			if !grew && addedFreshRegions == 0 {
				logf("⚠  all VMSS at per-VMSS hard cap (%d); cannot scale further", perVMSSHardCap)
				return
			}
		}
	}

	st.mu.Lock()
	st.scaleCooldownUntil = time.Now().Add(90 * time.Second)
	st.mu.Unlock()
}

func nextVMSSCapacity(current, requestedExtra, remainingBudget int) int {
	if requestedExtra <= 0 || remainingBudget <= 0 || current >= perVMSSHardCap {
		return current
	}
	return min(perVMSSHardCap, current+min(requestedExtra, remainingBudget))
}

// pickFreshRegion returns the next candidate region that (a) has the SKU
// available in the cache, (b) is not already in use, and (c) has not been
// excluded. Empty string means "no fresh region available".
func pickFreshRegion(
	cache *skucache.File,
	vmSize string,
	used map[string]bool,
	excluded map[string]bool,
) string {
	available := map[string]bool{}
	for _, r := range cache.AvailableRegions(vmSize) {
		available[skucache.NormaliseRegion(r)] = true
	}
	for _, r := range defaultCandidateRegions {
		key := skucache.NormaliseRegion(r)
		if !available[key] {
			continue
		}
		if used[r] || (excluded != nil && excluded[r]) {
			continue
		}
		return r
	}
	return ""
}

// addRegionVMSS deploys a single-region VMSS into the existing resource group
// via fleet-add-region.bicep. Returns the new VMSS name.
func addRegionVMSS(
	ctx context.Context,
	az *azcli.Runner,
	profile *config.AttackProfile,
	spec *runspec.RunSpec,
	rg string,
	region string,
	capacity int,
	bicepDir string,
	customData string,
	uamiResourceID string,
	uamiClientID string,
) (string, error) {
	params := map[string]any{
		"runName":          profile.RunName,
		"runSuffix":        spec.RunSuffix,
		"region":           region,
		"vmSize":           profile.Fleet.VMSize,
		"capacity":         capacity,
		"maxVMCount":       profile.Fleet.MaxVMCount,
		"useSpot":          profile.Fleet.UseSpot,
		"adminUsername":    "ddos",
		"sshPublicKey":     strings.TrimSpace(spec.SSHPublicKeyOpen),
		"customDataBase64": customData,
		"autoDeleteAt":     spec.AutoDeleteAt.Format(time.RFC3339),
		"owner":            spec.Owner,
		"uamiResourceId":   uamiResourceID,
		"uamiClientId":     uamiClientID,
	}
	deployName := fmt.Sprintf("ddos-add-%s-%s-%d", spec.RunSuffix, region, time.Now().Unix())
	outs, err := bicepdeploy.ResourceGroupScope(ctx, az, rg, bicepdeploy.Options{
		Name:         deployName,
		TemplateFile: filepath.Join(bicepDir, "fleet-add-region.bicep"),
		Parameters:   params,
	})
	if err != nil {
		return "", err
	}
	name := outs.String("vmssName")
	if name == "" {
		return "", fmt.Errorf("addRegionVMSS: bicep deploy returned empty vmssName")
	}
	return name, nil
}

// refillEvictions scales up a VMSS when instances have been evicted.
func refillEvictions(ctx context.Context, az *azcli.Runner, rg, vmssName string, target, max int) error {
	instances, err := az.VMSSListInstances(ctx, rg, vmssName)
	if err != nil {
		return err
	}
	// Count non-deleting instances as the effective capacity.
	live := 0
	for _, inst := range instances {
		switch inst.ProvisioningState {
		case "Succeeded", "Provisioning", "Creating", "Updating":
			live++
		}
	}
	if live < target {
		newCap := target
		if newCap > max {
			newCap = max
		}
		logf("spot eviction: %s live=%d target=%d → scaling to %d", vmssName, live, target, newCap)
		return az.VMSSScale(ctx, rg, vmssName, newCap)
	}
	return nil
}

// foundationParams builds the Bicep parameter map for foundation.bicep.
func foundationParams(p *config.AttackProfile, spec *runspec.RunSpec, uc *userconfig.File, deployerObjectID string) map[string]any {
	return map[string]any{
		"runName":                    p.RunName,
		"runSuffix":                  spec.RunSuffix,
		"autoDeleteAt":               spec.AutoDeleteAt.Format(time.RFC3339),
		"owner":                      spec.Owner,
		"maxVMCount":                 p.Fleet.MaxVMCount,
		"observabilityResourceGroup": uc.Observability.ResourceGroup,
		"dcrName":                    dcrShortName(uc.Observability.DCRResourceID),
		"deployerObjectId":           deployerObjectID,
	}
}

// fleetParams builds the Bicep parameter map for fleet.bicep.
func fleetParams(p *config.AttackProfile, spec *runspec.RunSpec, customDataBase64, uamiResourceID, uamiClientID string) map[string]any {
	return map[string]any{
		"runName":          p.RunName,
		"runSuffix":        spec.RunSuffix,
		"regions":          p.Fleet.Regions,
		"vmSize":           p.Fleet.VMSize,
		"initialVMCount":   p.Fleet.InitialVMCount,
		"maxVMCount":       p.Fleet.MaxVMCount,
		"useSpot":          p.Fleet.UseSpot,
		"adminUsername":    "ddos",
		"sshPublicKey":     strings.TrimSpace(spec.SSHPublicKeyOpen),
		"customDataBase64": customDataBase64,
		"autoDeleteAt":     spec.AutoDeleteAt.Format(time.RFC3339),
		"owner":            spec.Owner,
		"uamiResourceId":   uamiResourceID,
		"uamiClientId":     uamiClientID,
	}
}

// buildRemoteWriteURL assembles the DCE ingestion URL from user config.
func buildRemoteWriteURL(uc *userconfig.File) (string, error) {
	if uc.Observability.DCEURL == "" || uc.Observability.DCRImmutableID == "" {
		return "", fmt.Errorf("observability.dceUrl and dcrImmutableId must be set in ~/.ddos/config.yaml; run `ddos init-observability` first")
	}
	return fmt.Sprintf("%s/dataCollectionRules/%s/streams/Microsoft-PrometheusMetrics/api/v1/write?api-version=2023-04-24",
		strings.TrimRight(uc.Observability.DCEURL, "/"),
		uc.Observability.DCRImmutableID,
	), nil
}

// quotaFilePath resolves the region-quota.yaml path relative to the running binary
// (for installed binaries) and falls back to the CWD (for `go run` from repo root).
func quotaFilePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "region-quota.yaml"
	}
	candidates := []string{
		filepath.Join(filepath.Dir(exe), "region-quota.yaml"),
		"region-quota.yaml",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "region-quota.yaml"
}

// resolveRegionsFromQuota filters requested regions against the quota file and
// returns a list of regions where the vmSize has available quota, padded with
// additional candidate regions when necessary.
func resolveRegionsFromQuota(
	qcache *quotacache.File,
	vmSize string,
	requested []string,
	exclude map[string]bool,
) ([]string, error) {
	pool := uniqueOrderedStrings(append(append([]string{}, requested...), defaultCandidateRegions...))
	// Empty requested means "auto-expand": return every region with available quota.
	// Non-empty means "use exactly these N regions" (substituting unavailable ones).
	maxCount := len(requested)
	if maxCount == 0 {
		maxCount = len(pool)
	}
	final := make([]string, 0, maxCount)
	used := map[string]bool{}
	for _, r := range pool {
		key := quotacache.NormaliseRegion(r)
		if exclude != nil && exclude[key] {
			continue
		}
		if used[key] {
			continue
		}
		if qcache.AvailableVMs(key, vmSize) <= 0 {
			continue
		}
		final = append(final, key)
		used[key] = true
		if len(final) >= maxCount {
			break
		}
	}
	if len(final) == 0 {
		return nil, fmt.Errorf("%s has no available quota in any of %d candidate regions (excluded: %v)",
			vmSize, len(pool), keysOf(exclude))
	}
	return final, nil
}

// resolveRegions is the dispatch function: uses qcache if present, falls back to
// skucache, or returns requested regions unfiltered when neither is available.
// An empty requested slice means "auto-expand to all available regions"; a cache
// is required in that case (error returned if neither cache is present).
func resolveRegions(
	qcache *quotacache.File,
	cache *skucache.File,
	vmSize string,
	requested []string,
	exclude map[string]bool,
) ([]string, error) {
	if qcache != nil {
		return resolveRegionsFromQuota(qcache, vmSize, requested, exclude)
	}
	if cache != nil {
		return resolveRegionsFromCache(cache, vmSize, requested, exclude)
	}
	// Neither cache present — cannot auto-expand an empty list.
	if len(requested) == 0 {
		return nil, fmt.Errorf("fleet.regions is empty and no quota/SKU cache is available; run `ddos refresh-quota` first")
	}
	return requested, nil
}

// bicepDirectory resolves the infra/bicep path relative to the running binary.
// Falls back to a development-time path so `go run` works from the repo root.
func bicepDirectory() string {
	exe, err := os.Executable()
	if err != nil {
		return "infra/bicep"
	}
	// When running from the repo root via `go run ./cmd/ddos`, the working
	// directory is the repo root. When running the installed binary, look
	// next to the binary first.
	candidates := []string{
		filepath.Join(filepath.Dir(exe), "infra", "bicep"),
		"infra/bicep",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "infra/bicep"
}

// dcrShortName extracts the final segment of an ARM resource ID.
func dcrShortName(id string) string {
	id = strings.TrimRight(id, "/")
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			return id[i+1:]
		}
	}
	return id
}

// logf prints a timestamped log line to stdout.
func logf(format string, args ...any) {
	ts := time.Now().UTC().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	var line string
	if runtime.GOOS == "windows" {
		line = fmt.Sprintf("[%s] %s\r\n", ts, msg)
	} else {
		line = fmt.Sprintf("[%s] %s\n", ts, msg)
	}
	fmt.Print(line)
	runLogMu.Lock()
	if runLogFile != nil {
		// Always write Unix-style newlines to the file regardless of OS so the
		// log is easy to grep on any machine.
		fileLine := line
		if runtime.GOOS == "windows" {
			fileLine = fmt.Sprintf("[%s] %s\n", ts, msg)
		}
		_, _ = io.WriteString(runLogFile, fileLine)
	}
	runLogMu.Unlock()
}

// ---------------------------------------------------------------------------
// Per-test log file.
//
// Every `ddos run` writes its full transcript to
//   runs/<run-name>-<suffix>/run.log
// alongside a snapshot of the source attack profile (profile.yaml). The
// directory and file persist after cleanup so previous test results can be
// inspected at any time.
// ---------------------------------------------------------------------------

var (
	runLogMu   sync.Mutex
	runLogFile *os.File
	runLogDir  string
)

// openRunLog creates runs/<runName>-<suffix>/ and opens run.log for append.
// Silent on failure — logging to stdout still works.
func openRunLog(runName, suffix string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cwd, "runs", fmt.Sprintf("%s-%s", runName, suffix))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	logPath := filepath.Join(dir, "run.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	runLogMu.Lock()
	runLogFile = f
	runLogDir = dir
	runLogMu.Unlock()
	// Banner so concatenated logs are easy to split if the same dir is reused.
	header := fmt.Sprintf("\n========== ddos run @ %s (UTC) ==========\n",
		time.Now().UTC().Format(time.RFC3339))
	_, _ = io.WriteString(f, header)
	return dir, nil
}

// closeRunLog flushes and closes the per-test log file. Safe to call twice.
func closeRunLog() {
	runLogMu.Lock()
	defer runLogMu.Unlock()
	if runLogFile != nil {
		_ = runLogFile.Close()
		runLogFile = nil
	}
}

// snapshotProfile copies the source attack profile into the run dir so each
// test directory is self-contained for later inspection.
func snapshotProfile(profilePath, dstDir string) {
	if dstDir == "" {
		return
	}
	src, err := os.Open(profilePath)
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(dstDir, "profile.yaml"))
	if err != nil {
		return
	}
	defer dst.Close()
	_, _ = io.Copy(dst, src)
}

// defaultCandidateRegions is the fallback pool used for SKU resolution when a
// requested region is unavailable. Curated to span US/EU/APAC for diverse spot
// capacity pools. The actual availability check is done against the SKU cache,
// not Azure, so updating this list is cheap.
var defaultCandidateRegions = []string{
	"eastus", "eastus2", "westus2", "westus3",
	"southcentralus", "centralus", "northcentralus",
	"northeurope", "westeurope", "uksouth", "francecentral",
	"southeastasia", "japaneast", "australiaeast",
}

// resolveRegionsFromCache filters the requested regions (plus the built-in
// candidate pool) against the local SKU availability cache and returns the
// final region list. `exclude` lets the caller skip regions already known to
// be capacity-exhausted (e.g., after a SkuNotAvailable on a previous attempt).
//
// The returned slice has the same length as `requested` when possible;
// shorter only if the candidate pool itself is depleted. An error is only
// returned when zero regions remain.
func resolveRegionsFromCache(
	cache *skucache.File,
	vmSize string,
	requested []string,
	exclude map[string]bool,
) ([]string, error) {
	available := map[string]bool{}
	for _, r := range cache.AvailableRegions(vmSize) {
		available[skucache.NormaliseRegion(r)] = true
	}
	if len(available) == 0 {
		return nil, fmt.Errorf("%s is not offered in any region for subscription %s — try a different vmSize or refresh the cache",
			vmSize, cache.SubscriptionID)
	}

	// Build the candidate order: requested first, then the curated pool.
	pool := uniqueOrderedStrings(append(append([]string{}, requested...), defaultCandidateRegions...))

	// Empty requested means "auto-expand": return every available region.
	maxCount := len(requested)
	if maxCount == 0 {
		maxCount = len(pool)
	}
	final := make([]string, 0, maxCount)
	used := map[string]bool{}
	for _, r := range pool {
		key := skucache.NormaliseRegion(r)
		if !available[key] {
			continue
		}
		if exclude != nil && exclude[r] {
			continue
		}
		if used[r] {
			continue
		}
		final = append(final, r)
		used[r] = true
		if len(final) >= maxCount {
			break
		}
	}

	if len(final) == 0 {
		return nil, fmt.Errorf("%s not available in any of %d candidate regions (excluded: %v)",
			vmSize, len(pool), keysOf(exclude))
	}
	return final, nil
}

// isCapacityError reports whether the error from a fleet deployment looks like
// a dynamic capacity / spot availability problem worth retrying with different
// regions (vs. a real bug to surface).
func isCapacityError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SkuNotAvailable") ||
		strings.Contains(s, "AllocationFailed") ||
		strings.Contains(s, "OverconstrainedAllocationRequest") ||
		strings.Contains(s, "Capacity Restrictions") ||
		strings.Contains(s, "ZonalAllocationFailed")
}

// uniqueOrderedStrings de-duplicates a slice while preserving first-seen order.
func uniqueOrderedStrings(xs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// equalStringSlice reports whether a and b have the same elements in the same order.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// keysOf returns the keys of a string-keyed set for diagnostic output.
func keysOf(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
