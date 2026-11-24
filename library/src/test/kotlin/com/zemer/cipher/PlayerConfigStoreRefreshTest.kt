package com.zemer.cipher

import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.async
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong

/**
 * End-to-end (but network-free) coverage of the refresh timing contract:
 *
 *  - single-flight: the two refresh paths (unknown-hash forceRefresh, stream-rejection) share one
 *    lock and one fetch generation — concurrent triggers never produce duplicate requests and
 *    neither path starves the other (a queued caller piggybacks on the in-flight fetch's outcome);
 *  - cooldown semantics: a pure network failure (server never reached) does NOT consume a cooldown,
 *    any HTTP response does, and the two paths' cooldowns stay independent across the real paths;
 *  - wall-clock rollback: cooldowns and the cache TTL treat a future timestamp as expired instead
 *    of wedging for the whole clock skew;
 *  - bad tables (HTTP error / invalid JSON) keep the previous usable config, and 304 handling keeps
 *    the ETag metadata consistent.
 */
class PlayerConfigStoreRefreshTest {

    @get:Rule
    val tmp = TemporaryFolder()

    private lateinit var cacheDir: File

    private val now = AtomicLong(1_000_000_000L)

    /** Scripted fetcher; each element is consumed on a successive call. */
    private class ScriptedFetcher(
        private val responses: MutableList<suspend (String?) -> PlayerConfigStore.RemoteFetch>,
    ) : PlayerConfigStore.ConfigFetcher {
        val calls = AtomicInteger(0)
        val lastIfNoneMatch = mutableListOf<String?>()

        override suspend fun fetch(ifNoneMatch: String?): PlayerConfigStore.RemoteFetch {
            lastIfNoneMatch += ifNoneMatch
            val index = calls.getAndIncrement()
            check(index < responses.size) { "unexpected fetch #${index + 1}, only ${responses.size} scripted" }
            return responses[index](ifNoneMatch)
        }
    }

    /**
     * A fetcher that parks the FIRST call mid-flight (signalling entry via [entered] until [gate]
     * is released); any later call answers immediately from [laterAnswers]. One instance therefore
     * drives both the in-flight fetch and the retries queued behind it.
     */
    private class ParkedFetcher(
        val entered: CompletableDeferred<Unit>,
        private val gate: CompletableDeferred<Unit>,
        private val firstAnswer: suspend (String?) -> PlayerConfigStore.RemoteFetch,
        private val laterAnswers: MutableList<suspend (String?) -> PlayerConfigStore.RemoteFetch>,
    ) : PlayerConfigStore.ConfigFetcher {
        val calls = AtomicInteger(0)

        override suspend fun fetch(ifNoneMatch: String?): PlayerConfigStore.RemoteFetch {
            val index = calls.getAndIncrement()
            if (index == 0) {
                entered.complete(Unit)
                gate.await()
                return firstAnswer(ifNoneMatch)
            }
            check(index <= laterAnswers.size) { "unexpected fetch #${index + 1}" }
            return laterAnswers[index - 1](ifNoneMatch)
        }
    }

    private fun parkedFetcher(
        firstAnswer: suspend (String?) -> PlayerConfigStore.RemoteFetch,
        vararg laterAnswers: suspend (String?) -> PlayerConfigStore.RemoteFetch,
    ): Pair<ParkedFetcher, CompletableDeferred<Unit>> {
        val entered = CompletableDeferred<Unit>()
        val gate = CompletableDeferred<Unit>()
        val fetcher = ParkedFetcher(entered, gate, firstAnswer, laterAnswers.toMutableList())
        PlayerConfigStore.fetcherForTest = fetcher
        return fetcher to gate
    }

    private fun tableJson(hash: String, sts: Int) =
        """{"schemaVersion":1,"players":{"$hash":{"sig":"mP(4,155,INPUT)","nClass":"Yx","sts":$sts}}}"""

    private fun ok(json: String, etag: String = "\"e1\""): suspend (String?) -> PlayerConfigStore.RemoteFetch =
        { PlayerConfigStore.RemoteFetch.Response(200, json, etag) }

    private val http404: suspend (String?) -> PlayerConfigStore.RemoteFetch =
        { PlayerConfigStore.RemoteFetch.Response(404, null, null) }

    private val http304: suspend (String?) -> PlayerConfigStore.RemoteFetch =
        { PlayerConfigStore.RemoteFetch.Response(304, null, "\"e-stored\"") }

    private val networkDown: suspend (String?) -> PlayerConfigStore.RemoteFetch =
        { PlayerConfigStore.RemoteFetch.NetworkFailure }

    private fun installFetcher(
        vararg responses: suspend (String?) -> PlayerConfigStore.RemoteFetch,
    ): ScriptedFetcher {
        val fetcher = ScriptedFetcher(responses.toMutableList())
        PlayerConfigStore.fetcherForTest = fetcher
        return fetcher
    }

    @Before
    fun setUp() {
        cacheDir = tmp.newFolder("cipher_cache")
        PlayerConfigStore.resetForTest()
        PlayerConfigStore.cacheDirForTest = cacheDir
        PlayerConfigStore.clockMs = { now.get() }
    }

    @After
    fun tearDown() {
        PlayerConfigStore.resetForTest()
    }

    // ---------------------------------------------------------------- single-flight

    @Test
    fun `concurrent unknown-hash refreshes collapse into one fetch and both callers learn the result`() =
        runBlocking {
            val hash = "abcd1234"
            val (fetcher, gate) = parkedFetcher(firstAnswer = {
                PlayerConfigStore.RemoteFetch.Response(200, tableJson(hash, 20613), "\"e1\"")
            })

            val first = async(Dispatchers.Default) { PlayerConfigStore.forceRefresh(hash) }
            fetcher.entered.await() // the first call now holds the single-flight lock inside the fetch
            val second = async(Dispatchers.Default) { PlayerConfigStore.forceRefresh(hash) }
            delay(100) // let the second call queue on the shared lock

            gate.complete(Unit)
            withTimeout(5_000) {
                assertTrue("the fetching caller gets the hash", first.await())
                assertTrue("the queued caller piggybacks and also gets the hash", second.await())
            }
            assertEquals("exactly one HTTP request for two concurrent triggers", 1, fetcher.calls.get())
            assertNotNull(PlayerConfigStore.get(hash))
        }

    @Test
    fun `a queued stream-rejection refresh piggybacks on the in-flight unknown-hash fetch`() =
        runBlocking {
            val hash = "abcd1234"
            val (fetcher, gate) = parkedFetcher(firstAnswer = {
                PlayerConfigStore.RemoteFetch.Response(200, tableJson(hash, 20613), "\"e1\"")
            })

            val forced = async(Dispatchers.Default) { PlayerConfigStore.forceRefresh(hash) }
            fetcher.entered.await()
            // The rejection path cannot short-circuit (its own hash may be present-but-wrong), so it
            // is a true second trigger that must not starve behind the shared lock.
            val rejection = async(Dispatchers.Default) { PlayerConfigStore.refreshAfterStreamRejection() }
            delay(100)

            gate.complete(Unit)
            withTimeout(5_000) {
                assertTrue(forced.await())
                assertTrue("rejection path adopts the changed-table outcome without a second request",
                    rejection.await())
            }
            assertEquals(1, fetcher.calls.get())
            // Adopting the outcome arms the REJECTION cooldown (its own stamp), so an immediate
            // retry is gated even though it never ran its own fetch.
            now.addAndGet(PlayerConfigStore.FORCE_REFRESH_COOLDOWN_MS - 1)
            assertFalse(PlayerConfigStore.refreshAfterStreamRejection())
            assertEquals("the piggybacked retry must not fetch", 1, fetcher.calls.get())
        }

    @Test
    fun `an in-flight rejection refresh does not starve a queued unknown-hash self-heal`() =
        runBlocking {
            val hash = "efef5678"
            val (fetcher, gate) = parkedFetcher(firstAnswer = {
                PlayerConfigStore.RemoteFetch.Response(200, tableJson(hash, 20620), "\"e2\"")
            })

            val rejection = async(Dispatchers.Default) { PlayerConfigStore.refreshAfterStreamRejection() }
            fetcher.entered.await()
            val forced = async(Dispatchers.Default) { PlayerConfigStore.forceRefresh(hash) }
            delay(100)

            gate.complete(Unit)
            withTimeout(5_000) {
                assertTrue(rejection.await())
                assertTrue("unknown-hash self-heal must not be starved by the rejection path", forced.await())
            }
            assertEquals(1, fetcher.calls.get())
        }

    // ---------------------------------------------------------------- cooldown semantics

    @Test
    fun `pure network failure does not consume either cooldown - immediate retries refetch`() =
        runBlocking {
            val hash = "abcd1234"
            val fetcher = installFetcher(networkDown, networkDown, networkDown, networkDown)

            assertFalse("network failure cannot bring the hash in", PlayerConfigStore.forceRefresh(hash))
            assertFalse(PlayerConfigStore.refreshAfterStreamRejection())
            // No time passes: without the network-failure exemption both paths would now be on
            // cooldown and these calls would skip without fetching.
            assertFalse(PlayerConfigStore.forceRefresh(hash))
            assertFalse(PlayerConfigStore.refreshAfterStreamRejection())

            assertEquals("all four triggers attempted a fetch; none was cooldown-gated", 4, fetcher.calls.get())
        }

    @Test
    fun `any HTTP response arms the cooldown - error status included`() = runBlocking {
        val hash = "abcd1234"
        val fetcher = installFetcher(http404, http404)

        assertFalse(PlayerConfigStore.forceRefresh(hash))
        // Still inside the cooldown window: no second request must go out.
        now.addAndGet(PlayerConfigStore.FORCE_REFRESH_COOLDOWN_MS - 1)
        assertFalse(PlayerConfigStore.forceRefresh(hash))
        assertEquals(1, fetcher.calls.get())

        // After the window elapses, the path is open again.
        now.addAndGet(2)
        assertFalse(PlayerConfigStore.forceRefresh(hash))
        assertEquals(2, fetcher.calls.get())
    }

    @Test
    fun `a network failure queued behind another fetch resets the queued path's cooldown too`() =
        runBlocking {
            val hash = "abcd1234"
            // First (parked) call never reaches the server; the un-gated retry gets a good table.
            val (fetcher, gate) = parkedFetcher(
                { PlayerConfigStore.RemoteFetch.NetworkFailure },
                ok(tableJson(hash, 20613)),
            )

            val forced = async(Dispatchers.Default) { PlayerConfigStore.forceRefresh(hash) }
            fetcher.entered.await()
            val rejection = async(Dispatchers.Default) { PlayerConfigStore.refreshAfterStreamRejection() }
            delay(100)
            gate.complete(Unit)
            withTimeout(5_000) {
                assertFalse(forced.await())
                assertFalse(rejection.await())
            }
            assertEquals(1, fetcher.calls.get())

            // Both paths saw "server unreachable" via the adopted outcome, so neither may be gated.
            assertTrue(PlayerConfigStore.forceRefresh(hash))
            assertEquals(2, fetcher.calls.get())
            assertNotNull(PlayerConfigStore.get(hash))
        }

    @Test
    fun `the two paths keep independent cooldowns through the real refresh entry points`() = runBlocking {
        val hash = "abcd1234"
        // Two responses: one per path. Neither response contains the requested hash.
        val fetcher = installFetcher(
            { PlayerConfigStore.RemoteFetch.Response(200, tableJson("11112222", 1), "\"a\"") },
            { PlayerConfigStore.RemoteFetch.Response(200, tableJson("33334444", 2), "\"b\"") },
        )

        assertFalse(PlayerConfigStore.forceRefresh(hash)) // arms ONLY the forced cooldown
        // NOT gated: the rejection path has its own stamp, even though forceRefresh just fetched.
        assertTrue(PlayerConfigStore.refreshAfterStreamRejection())
        assertEquals(2, fetcher.calls.get())

        // Now each is gated by its OWN cooldown.
        now.addAndGet(PlayerConfigStore.FORCE_REFRESH_COOLDOWN_MS - 1)
        assertFalse(PlayerConfigStore.forceRefresh(hash))
        assertFalse(PlayerConfigStore.refreshAfterStreamRejection())
        assertEquals("cooldown skips perform no fetch", 2, fetcher.calls.get())
    }

    // ----------------------------------------------------- clock rollback (cooldown + TTL)

    @Test
    fun `cooldown expires immediately when the wall clock is stepped backwards`() = runBlocking {
        val hash = "abcd1234"
        val fetcher = installFetcher(http404, http404)

        assertFalse(PlayerConfigStore.forceRefresh(hash))
        assertEquals(1, fetcher.calls.get())

        // Clock steps back past the armed stamp: the in-range window check sees a negative delta
        // and treats the cooldown as expired rather than holding it for the whole skew.
        now.addAndGet(-(PlayerConfigStore.FORCE_REFRESH_COOLDOWN_MS + 10_000))
        assertFalse(PlayerConfigStore.forceRefresh(hash))
        assertEquals("the backward step must reopen the path immediately", 2, fetcher.calls.get())
    }

    @Test
    fun `startup TTL treats a future persisted fetch stamp as stale after a clock rollback`() =
        runBlocking {
            val writtenAt = now.get()
            File(cacheDir, "configs_remote.meta").writeText("\"e1\"\n$writtenAt")
            val fetcher = installFetcher(http304)

            // Within the TTL: no fetch.
            now.addAndGet(PlayerConfigStore.REFRESH_TTL_MS - 1)
            PlayerConfigStore.refreshIfStaleForTest()
            assertEquals(0, fetcher.calls.get())

            // Clock rolled back behind the persisted stamp: a plain `now - stamp < ttl` check would
            // call this "fresh" for the entire skew; the in-range check must instead refetch.
            now.addAndGet(-PlayerConfigStore.REFRESH_TTL_MS - 60_000)
            PlayerConfigStore.refreshIfStaleForTest()
            assertEquals(1, fetcher.calls.get())
        }

    // ----------------------------------------------------------- invalid tables / 304 / cache

    @Test
    fun `an invalid remote table is rejected and the previous usable config survives`() = runBlocking {
        val hash = "abcd1234"
        // First a good table lands...
        val fetcher = installFetcher(
            ok(tableJson(hash, 20613)),
            // ...then a malformed publish (the host was reached, so the cooldown still arms).
            { PlayerConfigStore.RemoteFetch.Response(200, "{not json", "\"e-bad\"") },
        )

        // The rejection path always re-fetches (an entry can be present but WRONG), so unlike
        // forceRefresh it does not short-circuit on an already-present hash.
        assertTrue(PlayerConfigStore.refreshAfterStreamRejection())
        val before = PlayerConfigStore.get(hash)
        assertNotNull(before)
        val epochBefore = PlayerConfigStore.configEpoch

        now.addAndGet(PlayerConfigStore.FORCE_REFRESH_COOLDOWN_MS + 1)
        assertFalse("the malformed table changes nothing", PlayerConfigStore.refreshAfterStreamRejection())

        assertSameConfig(before, PlayerConfigStore.get(hash))
        assertEquals("a rejected table must not bump the epoch (no spurious rebuild)",
            epochBefore, PlayerConfigStore.configEpoch)
        assertEquals(2, fetcher.calls.get())
    }

    @Test
    fun `a 304 keeps the stored ETag and advances the fetch time without changing the table`() =
        runBlocking {
            File(cacheDir, "configs_remote.meta").writeText("\"e-stored\"\n${now.get() - 10_000}")
            val fetcher = installFetcher(http304)
            val epochBefore = PlayerConfigStore.configEpoch

            assertFalse(PlayerConfigStore.refreshAfterStreamRejection())

            assertEquals(1, fetcher.calls.get())
            assertEquals("the conditional request reused the stored ETag",
                "\"e-stored\"", fetcher.lastIfNoneMatch.single())
            val meta = File(cacheDir, "configs_remote.meta").readText().split("\n")
            assertEquals("\"e-stored\"", meta[0])
            assertEquals(now.get().toString(), meta[1])
            assertEquals(epochBefore, PlayerConfigStore.configEpoch)
        }

    @Test
    fun `a valid cached body beside an unreadable meta file discards the junk ETag`() {
        File(cacheDir, "configs_remote.json").writeText(tableJson("abcd1234", 20613))
        File(cacheDir, "configs_remote.meta").writeText("garbage-no-newline")

        PlayerConfigStore.applyCachedOverlay()

        assertNotNull("the readable body still overlays", PlayerConfigStore.get("abcd1234"))
        assertFalse("the unreadable ETag must not ride along into the next conditional request",
            File(cacheDir, "configs_remote.meta").exists())
    }

    private fun assertSameConfig(
        expected: FunctionNameExtractor.HardcodedPlayerConfig?,
        actual: FunctionNameExtractor.HardcodedPlayerConfig?,
    ) {
        assertNotNull(expected)
        assertNotNull(actual)
        assertEquals(expected, actual)
    }
}
