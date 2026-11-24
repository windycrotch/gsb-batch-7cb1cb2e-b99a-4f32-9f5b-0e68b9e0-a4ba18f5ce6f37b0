package com.zemer.cipher

import android.content.Context
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import okhttp3.Request
import timber.log.Timber
import java.io.File
import java.io.FileOutputStream

/**
 * Owns the player-config table at runtime: bundled asset as the offline default, overlaid
 * by the same JSON fetched from the zemer-cipher repo so rotated players are fixed without
 * an APK update. Parsing/validation is delegated to [PlayerConfigParser]; only validated
 * payloads ever replace the in-memory map or touch the disk cache.
 *
 * Read path is lock-free: lookups hit an immutable map behind a @Volatile reference that
 * refreshes swap wholesale.
 */
object PlayerConfigStore {
    private const val TAG = "Zemer_CipherConfig"
    private const val ASSET_NAME = "player_configs.json"
    private const val REMOTE_URL =
        "https://raw.githubusercontent.com/ZemerTeam/zemer-cipher/master/library/src/main/assets/player_configs.json"

    // Mirrors PlayerJsFetcher.CACHE_TTL_MS.
    internal const val REFRESH_TTL_MS = 6 * 60 * 60 * 1000L

    // Failure-triggered refreshes are rate-limited so a player that is unknown both locally
    // and remotely doesn't turn every song into a GitHub request.
    internal const val FORCE_REFRESH_COOLDOWN_MS = 5 * 60 * 1000L

    // Note: names must not start with "player_" — PlayerJsFetcher.writeToCache() and
    // invalidateCache() purge the player-JS files (player_* + current_hash.txt) from this
    // shared dir; these config files are deliberately spared so the ETag survives decipher
    // retries.
    private const val CACHE_FILE = "configs_remote.json"
    private const val META_FILE = "configs_remote.meta"

    @Volatile
    private var appContext: Context? = null

    @Volatile
    private var bundledConfigs: Map<String, FunctionNameExtractor.HardcodedPlayerConfig> = emptyMap()

    @Volatile
    private var mergedConfigs: Map<String, FunctionNameExtractor.HardcodedPlayerConfig> = emptyMap()

    // Advanced every time a remote refresh actually changes the table. The cipher records the epoch
    // its WebView was built under and rebuilds when this advances, so a corrected config for the
    // current player — arriving by any refresh path AFTER the WebView was built from a missing or
    // wrong entry — takes effect on the next decipher instead of being ignored until the process is
    // restarted. See DecipherBuildState / CipherDeobfuscator.getOrCreateWebView().
    @Volatile
    var configEpoch: Int = 0
        private set

    @Volatile
    private var lastForcedAttemptMs = 0L

    // Separate from lastForcedAttemptMs so a stream-rejection refresh (which fires on any 403,
    // including unrelated/expired-URL ones) can never arm a cooldown that blocks the unknown-hash
    // forceRefresh self-heal, or vice versa. They still serialize on refreshMutex (single-flight).
    @Volatile
    private var lastRejectionAttemptMs = 0L

    // The two cooldown gates read DIFFERENT stamps on purpose (see above). Routed through these
    // functions — which forceRefresh / refreshAfterStreamRejection actually call — so a unit test
    // can prove neither path is gated by the other's cooldown without touching the network.
    /**
     * True iff [stampMs] lies within [windowMs] of [now]. The in-range check (not a plain
     * `now - stamp < window`) matters: these are wall-clock stamps, and a backward clock
     * adjustment (NTP correction, manual change) makes the delta negative — a plain
     * less-than would then hold the window for the entire skew duration, wedging
     * cooldowns/TTLs exactly while playback is broken. Every wall-clock window check in
     * this module MUST go through this helper.
     */
    internal fun withinWindow(now: Long, stampMs: Long, windowMs: Long) =
        (now - stampMs) in 0 until windowMs

    internal fun forcedCooldownActive(now: Long) = withinWindow(now, lastForcedAttemptMs, FORCE_REFRESH_COOLDOWN_MS)

    internal fun rejectionCooldownActive(now: Long) = withinWindow(now, lastRejectionAttemptMs, FORCE_REFRESH_COOLDOWN_MS)

    // Test-only: arm/disarm a cooldown stamp without invoking the network refresh paths.
    internal fun armForcedCooldownForTest(ms: Long) { lastForcedAttemptMs = ms }

    internal fun armRejectionCooldownForTest(ms: Long) { lastRejectionAttemptMs = ms }

    /**
     * Wall-clock source for every timestamp this object writes or compares. Routed through a
     * seam so unit tests can drive cooldown/TTL behavior — including a backward clock step —
     * deterministically. Production always reads the real system clock.
     */
    @Volatile
    internal var clockMs: () -> Long = { System.currentTimeMillis() }

    /**
     * Result of one remote-fetch transport attempt. [NetworkFailure] means the server was never
     * reached (DNS/connect/timeout/TLS) and therefore must NOT consume a cooldown; any
     * [Response] — 200, 304, 4xx, 5xx — did reach the host and arms the calling path's cooldown.
     */
    internal sealed interface RemoteFetch {
        data object NetworkFailure : RemoteFetch
        data class Response(val code: Int, val body: String?, val etag: String?) : RemoteFetch
    }

    /**
     * Transport seam: production uses OkHttp ([performHttpFetch]); unit tests install a fake.
     * Suspending so a test can park a fetch mid-flight (to exercise single-flight piggybacking)
     * without blocking a dispatcher thread; the production implementation is the ordinary
     * blocking OkHttp call, executed on Dispatchers.IO by the callers.
     */
    internal fun interface ConfigFetcher {
        suspend fun fetch(ifNoneMatch: String?): RemoteFetch
    }

    @Volatile
    internal var fetcherForTest: ConfigFetcher? = null

    private val refreshMutex = Mutex()
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    // Monotonic fetch generation. A caller snapshots it BEFORE acquiring [refreshMutex]; if it
    // advanced by the time the lock is held, another path's fetch landed during the wait and the
    // caller piggybacks on that outcome (see [adoptOutcomeIfFetchedDuringWait]) instead of issuing
    // a duplicate request to the same URL. Writes happen under the lock; the volatile read is only
    // a pre-lock snapshot.
    @Volatile
    private var fetchSerial: Long = 0L

    // Outcome of the most recent fetch. Guarded by refreshMutex.
    private class FetchOutcomeSummary(
        val serial: Long,
        val changed: Boolean,
        val reachedServer: Boolean,
        val completedAtMs: Long,
    )

    private var lastFetchOutcome: FetchOutcomeSummary? = null

    // One shared client for the whole library — see ZemerCipher.httpClient.
    private val httpClient: OkHttpClient
        get() = ZemerCipher.httpClient

    /**
     * Synchronous: loads the bundled asset and, if present and valid, the last-good cached
     * remote copy. Cheap (one bundled JSON asset + at most one cached file) and guarantees configs
     * exist before any lookup.
     */
    fun initialize(context: Context) {
        appContext = context.applicationContext

        bundledConfigs = when (val result = parseSource("bundled asset") { loadBundledJson(context) }) {
            null -> emptyMap()
            else -> result
        }
        if (bundledConfigs.isEmpty()) {
            Timber.tag(TAG).e("Bundled $ASSET_NAME missing or invalid — config table starts empty")
        } else {
            Timber.tag(TAG).d("Loaded bundled configs (${bundledConfigs.size} hashes)")
        }

        applyCachedOverlay()
    }

    /**
     * Overlays the last-good cached remote copy onto the bundled table. On ANY failure to
     * load it, the cache body AND the meta file are deleted together: an ETag surviving a
     * corrupt/missing body would make every subsequent conditional fetch 304 without a
     * re-download, locking the device on bundled-only configs until the remote content
     * happens to change. (No-op deletes on a clean first run.)
     */
    internal fun applyCachedOverlay() {
        val cached = parseSource("cached remote copy") { cacheFile()?.takeIf { it.exists() }?.readText() }
        mergedConfigs = if (cached != null) {
            Timber.tag(TAG).d("Overlaying cached remote configs (${cached.size} hashes)")
            // A readable body beside an unreadable meta means the ETag line is garbage: the next
            // conditional request would carry a junk If-None-Match. Drop the meta so that request
            // goes out unconditional and a fresh ETag gets written, instead of trusting the junk.
            val meta = metaFile()
            if (meta?.exists() == true && readMeta() == null) {
                Timber.tag(TAG).w("Cached meta unreadable beside a valid body — discarding stale ETag")
                meta.delete()
            }
            PlayerConfigParser.merge(bundledConfigs, cached)
        } else {
            cacheFile()?.delete()
            metaFile()?.delete()
            bundledConfigs
        }
    }

    /** Non-blocking TTL-gated refresh, kicked once from ZemerCipher.initialize(). */
    fun scheduleStartupRefresh() {
        scope.launch {
            try {
                refreshIfStale()
            } catch (e: Exception) {
                Timber.tag(TAG).w(e, "Startup config refresh failed: ${e.message}")
            }
        }
    }

    fun get(hash: String): FunctionNameExtractor.HardcodedPlayerConfig? {
        val configs = mergedConfigs
        if (configs.isEmpty()) {
            Timber.tag(TAG).w("Config table is empty (initialize not called or bundled asset broken)")
        }
        return configs[hash]
    }

    fun knownHashes(): Set<String> = mergedConfigs.keys

    /** Test-only: swaps the in-memory table without touching disk, context, or network. */
    internal fun setTableForTest(configs: Map<String, FunctionNameExtractor.HardcodedPlayerConfig>) {
        mergedConfigs = configs
    }

    /** Test-only: clears process-global state between JUnit tests. configEpoch deliberately stays. */
    internal fun resetForTest() {
        bundledConfigs = emptyMap()
        mergedConfigs = emptyMap()
        lastForcedAttemptMs = 0L
        lastRejectionAttemptMs = 0L
        fetchSerial = 0L
        lastFetchOutcome = null
        fetcherForTest = null
        clockMs = { System.currentTimeMillis() }
        cacheDirForTest = null
        appContext = null
    }

    /**
     * Failure-triggered refresh: called when a player-hash lookup misses. Single-flight, with
     * the cooldown decided under the lock (a check-then-set outside it would let concurrent
     * misses race). Returns true iff [missingHash] is now in the table — whether THIS call's
     * fetch did the work or a concurrent/just-finished refresh already brought it in — so
     * callers retry extraction exactly when it can succeed.
     */
    suspend fun forceRefresh(missingHash: String): Boolean = withContext(Dispatchers.IO) {
        val serialAtEntry = fetchSerial
        refreshMutex.withLock {
            // A refresh that held the lock while we waited (startup TTL, another miss) may
            // have just landed this config — don't burn a fetch or arm the cooldown.
            if (mergedConfigs.containsKey(missingHash)) {
                Timber.tag(TAG).d("forceRefresh: $missingHash arrived via concurrent refresh")
                return@withLock true
            }

            val now = clockMs()
            if (forcedCooldownActive(now)) {
                Timber.tag(TAG).d("forceRefresh skipped (cooldown)")
                return@withLock false
            }

            // A fetch that completed while we queued on the lock already answered this call:
            // adopt its outcome (and its cooldown semantics) rather than duplicate the request.
            adoptOutcomeIfFetchedDuringWait(
                serialAtEntry,
                armStamp = { lastForcedAttemptMs = it },
                resetStamp = { lastForcedAttemptMs = 0L },
            )?.let { outcome ->
                Timber.tag(TAG).d("forceRefresh piggybacked on a fetch that finished during the lock wait")
                return@withLock outcome.changed && mergedConfigs.containsKey(missingHash)
            }

            lastForcedAttemptMs = now
            val result = fetchAndApply()
            if (!result.reachedServer) {
                // The host was never contacted — don't consume the cooldown on a pure network
                // failure, so a rotation hit while briefly offline retries on the next trigger.
                lastForcedAttemptMs = 0L
            }
            mergedConfigs.containsKey(missingHash)
        }
    }

    /**
     * Stream-rejection refresh: a deciphered URL was rejected by the CDN (e.g. a WEB_REMIX 403),
     * which can mean the cipher produced a wrong-but-non-throwing signature from a stale/wrong
     * player config — a failure the [CipherDeobfuscator] exception-retry never sees. Unlike
     * [forceRefresh], this does NOT short-circuit when the current hash is already present (the
     * entry may be present but WRONG), so it always re-fetches unless gated; it has its OWN
     * cooldown (so it can't starve the unknown-hash [forceRefresh] path) but shares the
     * single-flight lock. Returns whether the table changed — when true, [configEpoch] has
     * advanced and the cipher rebuilds.
     */
    suspend fun refreshAfterStreamRejection(): Boolean = withContext(Dispatchers.IO) {
        val serialAtEntry = fetchSerial
        refreshMutex.withLock {
            val now = clockMs()
            if (rejectionCooldownActive(now)) {
                Timber.tag(TAG).d("refreshAfterStreamRejection skipped (cooldown)")
                return@withLock false
            }

            adoptOutcomeIfFetchedDuringWait(
                serialAtEntry,
                armStamp = { lastRejectionAttemptMs = it },
                resetStamp = { lastRejectionAttemptMs = 0L },
            )?.let { outcome ->
                Timber.tag(TAG).d("refreshAfterStreamRejection piggybacked on a fetch that finished during the lock wait")
                return@withLock outcome.changed
            }

            lastRejectionAttemptMs = now
            val result = fetchAndApply()
            if (!result.reachedServer) lastRejectionAttemptMs = 0L
            result.changed
        }
    }

    /**
     * If a fetch completed while this caller waited for [refreshMutex] (the serial advanced past
     * [serialAtEntry]), adopt its outcome: arm this path's cooldown when the server was reached,
     * leave the stamp disarmed on a pure network failure — exactly the semantics a self-performed
     * fetch would have produced. Returns null when nothing was fetched during the wait, so the
     * caller proceeds to its own gated fetch. Must be called under the lock.
     */
    private fun adoptOutcomeIfFetchedDuringWait(
        serialAtEntry: Long,
        armStamp: (Long) -> Unit,
        resetStamp: () -> Unit,
    ): FetchOutcomeSummary? {
        val outcome = lastFetchOutcome ?: return null
        if (outcome.serial <= serialAtEntry) return null
        if (outcome.reachedServer) armStamp(outcome.completedAtMs) else resetStamp()
        return outcome
    }

    private suspend fun refreshIfStale() {
        val lastFetchMs = readMeta()?.second ?: 0L
        val now = clockMs()
        // lastFetchMs is persisted, so a future stamp (wall clock stepped back after the
        // write) must count as stale, not fresh — withinWindow handles that.
        if (withinWindow(now, lastFetchMs, REFRESH_TTL_MS)) {
            Timber.tag(TAG).d("Remote configs fresh (fetched ${now - lastFetchMs} ms ago)")
            return
        }
        withContext(Dispatchers.IO) {
            refreshMutex.withLock { fetchAndApply() }
        }
    }

    /** Test-only entry point for the TTL gate (the production caller is scheduleStartupRefresh). */
    internal suspend fun refreshIfStaleForTest() = refreshIfStale()

    private class FetchResult(val changed: Boolean, val reachedServer: Boolean)

    /**
     * Fetches the remote JSON (with If-None-Match) and applies it when valid. Any failure —
     * HTTP error (including the 404 served until the file lands on the repo's default
     * branch), network exception, or validation failure — keeps the previous map and cache.
     * lastFetchMs is only advanced on 200/304 so transient failures retry on the next trigger.
     * Records a [FetchOutcomeSummary] under the lock so callers that queued during this fetch
     * can piggyback instead of repeating the request.
     */
    private suspend fun fetchAndApply(): FetchResult {
        val ifNoneMatch = readMeta()?.first
        val remoteFetch = fetcherForTest?.fetch(ifNoneMatch) ?: performHttpFetch(ifNoneMatch)

        val result = when (remoteFetch) {
            is RemoteFetch.NetworkFailure -> {
                Timber.tag(TAG).w("Remote config fetch never reached the server — keeping previous configs")
                FetchResult(changed = false, reachedServer = false)
            }
            is RemoteFetch.Response -> handleResponse(remoteFetch, ifNoneMatch)
        }

        lastFetchOutcome = FetchOutcomeSummary(
            serial = ++fetchSerial,
            changed = result.changed,
            reachedServer = result.reachedServer,
            completedAtMs = clockMs(),
        )
        return result
    }

    private fun handleResponse(response: RemoteFetch.Response, requestEtag: String?): FetchResult {
        // Any HTTP response — including error codes — means the host was reached, so the calling
        // path's cooldown must arm. Only a transport-level failure leaves it disarmed.
        if (response.code == 304) {
            Timber.tag(TAG).d("Remote configs unchanged (304)")
            writeMeta(response.etag ?: requestEtag.orEmpty(), clockMs())
            return FetchResult(changed = false, reachedServer = true)
        }
        if (response.code !in 200..299) {
            Timber.tag(TAG).w("Remote config fetch HTTP ${response.code} — keeping previous configs")
            return FetchResult(changed = false, reachedServer = true)
        }

        val body = response.body
        if (body.isNullOrEmpty()) {
            Timber.tag(TAG).w("Remote config fetch returned empty body — keeping previous configs")
            return FetchResult(changed = false, reachedServer = true)
        }

        val remote = when (val parsed = PlayerConfigParser.parse(body)) {
            is PlayerConfigParser.ParseResult.Failure -> {
                // A new table that fails validation must never displace the last-good one —
                // keeping the previous map/cache is what prevents a bad publish from locking
                // the device onto a broken or empty table.
                Timber.tag(TAG).w("Remote configs rejected: ${parsed.reason} — keeping previous configs")
                return FetchResult(changed = false, reachedServer = true)
            }
            is PlayerConfigParser.ParseResult.Success -> {
                if (parsed.skippedEntries.isNotEmpty()) {
                    Timber.tag(TAG).w("Remote configs: skipped invalid entries ${parsed.skippedEntries}")
                }
                parsed.configs
            }
        }

        val changed = applyRemote(remote, body, response.etag.orEmpty())
        return FetchResult(changed, reachedServer = true)
    }

    private fun performHttpFetch(ifNoneMatch: String?): RemoteFetch {
        return try {
            val request = Request.Builder()
                .url(REMOTE_URL)
                .header("User-Agent", "Mozilla/5.0")
                .apply { if (!ifNoneMatch.isNullOrEmpty()) header("If-None-Match", ifNoneMatch) }
                .build()

            httpClient.newCall(request).execute().use { response ->
                RemoteFetch.Response(
                    code = response.code,
                    body = response.body?.string(),
                    etag = response.header("ETag"),
                )
            }
        } catch (e: Exception) {
            Timber.tag(TAG).w(e, "Remote config fetch failed: ${e.message} — keeping previous configs")
            RemoteFetch.NetworkFailure
        }
    }

    /**
     * Applies a validated remote table to memory FIRST, then best-effort persists the raw
     * body + meta. A disk failure (full disk, IO error) must never discard an in-hand
     * validated fix — losing the cache only costs a refetch on the next start, while losing
     * the memory update costs working playback now. Returns whether the table changed.
     */
    internal fun applyRemote(
        remote: Map<String, FunctionNameExtractor.HardcodedPlayerConfig>,
        body: String,
        etag: String,
    ): Boolean {
        val merged = PlayerConfigParser.merge(bundledConfigs, remote)
        val changed = merged != mergedConfigs
        mergedConfigs = merged
        if (changed) configEpoch++
        Timber.tag(TAG).d("Remote configs applied (${remote.size} hashes, merged=${merged.size}, changed=$changed, epoch=$configEpoch)")

        try {
            cacheFile()?.let { writeAtomic(it, body) }
            writeMeta(etag, clockMs())
        } catch (e: Exception) {
            Timber.tag(TAG).w(e, "Could not persist remote configs (kept in memory): ${e.message}")
        }
        return changed
    }

    private fun parseSource(
        label: String,
        read: () -> String?,
    ): Map<String, FunctionNameExtractor.HardcodedPlayerConfig>? {
        val text = try {
            read() ?: return null
        } catch (e: Exception) {
            Timber.tag(TAG).w(e, "Could not read $label: ${e.message}")
            return null
        }
        return when (val result = PlayerConfigParser.parse(text)) {
            is PlayerConfigParser.ParseResult.Failure -> {
                Timber.tag(TAG).w("Rejected $label: ${result.reason}")
                null
            }
            is PlayerConfigParser.ParseResult.Success -> {
                if (result.skippedEntries.isNotEmpty()) {
                    Timber.tag(TAG).w("$label: skipped invalid entries ${result.skippedEntries}")
                }
                result.configs
            }
        }
    }

    private fun loadBundledJson(context: Context): String? =
        context.assets.open(ASSET_NAME).bufferedReader().use { it.readText() }

    // Test seam: unit tests point this at a temp dir; production resolves from appContext.
    internal var cacheDirForTest: File? = null

    private fun cacheDir(): File? {
        cacheDirForTest?.let { return it.apply { if (!exists()) mkdirs() } }
        val context = appContext ?: return null
        return File(context.filesDir, "cipher_cache").apply { if (!exists()) mkdirs() }
    }

    private fun cacheFile(): File? = cacheDir()?.let { File(it, CACHE_FILE) }

    private fun metaFile(): File? = cacheDir()?.let { File(it, META_FILE) }

    /** Meta file: line 1 = ETag (may be empty), line 2 = lastFetchMs. */
    private fun readMeta(): Pair<String, Long>? {
        return try {
            val file = metaFile()?.takeIf { it.exists() } ?: return null
            val lines = file.readText().split("\n")
            if (lines.size < 2) return null
            val lastFetchMs = lines[1].toLongOrNull() ?: return null
            lines[0] to lastFetchMs
        } catch (e: Exception) {
            null
        }
    }

    private fun writeMeta(etag: String, lastFetchMs: Long) {
        try {
            metaFile()?.let { writeAtomic(it, "$etag\n$lastFetchMs") }
        } catch (e: Exception) {
            Timber.tag(TAG).w(e, "Could not write config meta: ${e.message}")
        }
    }

    /**
     * Temp-file + fsync + rename so a process death mid-write can't leave a truncated file
     * (a corrupt cache body beside a valid ETag is exactly the 304-lock state
     * [applyCachedOverlay] defends against). The bytes are forced to disk BEFORE the rename:
     * rename is atomic, but without the sync a kill between rename and flush could expose an
     * empty or short target.
     */
    internal fun writeAtomic(file: File, content: String) {
        val tmp = File(file.parentFile, "${file.name}.tmp")
        FileOutputStream(tmp).use { stream ->
            stream.write(content.toByteArray(Charsets.UTF_8))
            stream.flush()
            stream.fd.sync()
        }
        if (!tmp.renameTo(file)) {
            // renameTo won't overwrite an existing target on some filesystems (notably Windows) —
            // retry after deleting it (two cheap metadata ops, still atomic) before the last-resort
            // direct write, which is both non-atomic and a second full write of the content.
            file.delete()
            if (!tmp.renameTo(file)) {
                file.writeText(content)
                tmp.delete()
            }
        }
    }
}
