package com.zemer.cipher

/**
 * Pure-JVM (no Android deps) state machine for the cipher WebView build lifecycle. It owns the
 * answer to one question: *is the cached decipher instance still built from the live config
 * table?*
 *
 * The timing contract it enforces:
 *
 *  - The live [PlayerConfigStore.configEpoch] is snapshotted BEFORE a build starts
 *    ([beginBuild]), not after it. A multi-second build can therefore never claim to
 *    incorporate a config refresh that landed while it was running: the next decipher sees
 *    [builtConfigEpoch] behind the live epoch and rebuilds — a refresh observed during a build
 *    is never swallowed.
 *  - Only a build that reaches [onBuildSucceeded] publishes a usable instance and an epoch.
 *    [onBuildFailed] (extraction/network failure) and [onInstanceDiscarded] (renderer death)
 *    leave no half-built instance cached, so a failed build can never masquerade as a working
 *    environment.
 *
 * Callers pass the live epoch in (like [RendererRecoveryPolicy] takes a clock value) so the
 * whole state machine is deterministically unit-testable.
 */
class DecipherBuildState {

    /** Epoch the currently cached instance was built under; -1 when no good instance exists. */
    var builtConfigEpoch: Int = -1
        private set

    /** True between [beginBuild] and one of the terminal outcomes. */
    var buildInProgress: Boolean = false
        private set

    /** Live epoch observed when the in-flight (or most recent) build started. */
    var epochAtBuildStart: Int = -1
        private set

    /** Whether a fully-built, not-yet-discarded instance exists to potentially reuse. */
    var instanceUsable: Boolean = false
        private set

    /** Player hash the cached instance was built for, or null when no instance is cached. */
    var currentPlayerHash: String? = null
        private set

    /**
     * Reuse is valid only when an instance exists, it was built under the [liveEpoch] table,
     * and the caller did not request a forced rebuild. A stale epoch (a refresh landed after
     * the build — including one that landed DURING the build, see [beginBuild]) forces a
     * rebuild on the next decipher instead of pinning playback to the old table for the
     * process lifetime.
     */
    fun canReuseInstance(liveEpoch: Int, forceRefresh: Boolean): Boolean =
        !forceRefresh && instanceUsable && builtConfigEpoch == liveEpoch

    /**
     * Record the start of a (multi-second) build against the table as it exists RIGHT NOW.
     * Snapshotting before extraction/loading is load-bearing: any refresh landing before
     * [onBuildSucceeded] leaves [builtConfigEpoch] short of the live epoch.
     */
    fun beginBuild(liveEpoch: Int) {
        epochAtBuildStart = liveEpoch
        buildInProgress = true
    }

    /**
     * Publish a successfully built instance. [incorporatedEpoch] is normally the
     * [epochAtBuildStart] snapshot; a same-thread forced refresh whose new config was fully
     * re-extracted may pass the newer epoch it actually incorporated. Anything newer than that
     * still arrived too late and remains visible as an epoch mismatch on the next call.
     */
    fun onBuildSucceeded(playerHash: String, incorporatedEpoch: Int) {
        check(buildInProgress) { "onBuildSucceeded called without a matching beginBuild" }
        builtConfigEpoch = incorporatedEpoch
        currentPlayerHash = playerHash
        instanceUsable = true
        buildInProgress = false
    }

    /**
     * The build failed (player JS unavailable, extraction produced nothing usable). No
     * half-built instance is published: [instanceUsable] stays false and the previously built
     * epoch is not advanced, so the next call attempts a fresh build rather than trusting a
     * partial environment.
     */
    fun onBuildFailed() {
        buildInProgress = false
        instanceUsable = false
        currentPlayerHash = null
    }

    /**
     * The cached instance died (renderer process gone) or was explicitly closed. A zombie
     * instance must never be reused regardless of epoch match, so the built epoch is reset:
     * the next call rebuilds even if the config table never changed.
     */
    fun onInstanceDiscarded() {
        instanceUsable = false
        currentPlayerHash = null
        builtConfigEpoch = -1
    }
}
