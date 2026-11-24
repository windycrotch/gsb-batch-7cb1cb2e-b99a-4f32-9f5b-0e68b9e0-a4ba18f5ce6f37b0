package com.zemer.cipher

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The cipher WebView build lifecycle, without a WebView. These pin the timings the task calls out:
 *
 *  - a config refresh observed WHILE a build is running must not be swallowed — the next decipher
 *    has to rebuild (epoch snapshotted BEFORE the build);
 *  - a failed build or a dead renderer must never leave a half-built instance cached as usable.
 */
class DecipherBuildStateTest {

    @Test
    fun `fresh state offers no reusable instance`() {
        val state = DecipherBuildState()
        assertFalse(state.canReuseInstance(liveEpoch = 0, forceRefresh = false))
        assertFalse(state.instanceUsable)
        assertNull(state.currentPlayerHash)
        assertEquals(-1, state.builtConfigEpoch)
    }

    @Test
    fun `a completed build is reusable only while the live epoch matches`() {
        val state = DecipherBuildState()

        state.beginBuild(liveEpoch = 0)
        state.onBuildSucceeded(playerHash = "abcd1234", incorporatedEpoch = 0)

        assertTrue(state.canReuseInstance(liveEpoch = 0, forceRefresh = false))
        assertEquals("abcd1234", state.currentPlayerHash)

        // A refresh landed after the build: the cached environment predates the live table.
        assertFalse("stale epoch must force a rebuild", state.canReuseInstance(liveEpoch = 1, forceRefresh = false))
        // An explicit force-refresh rebuilds even with a matching epoch.
        assertFalse(state.canReuseInstance(liveEpoch = 0, forceRefresh = true))
    }

    @Test
    fun `refresh landing during the build is detected on the next decipher and triggers a rebuild`() {
        val state = DecipherBuildState()

        // Build starts against table epoch 0...
        state.beginBuild(liveEpoch = 0)

        // ...another thread applies a refreshed config table mid-build (epoch advances to 1).
        // The build finishes having incorporated only epoch-0 configs.
        state.onBuildSucceeded(playerHash = "abcd1234", incorporatedEpoch = 0)

        assertFalse(
            "a refresh observed during the build must not be swallowed — next decipher rebuilds",
            state.canReuseInstance(liveEpoch = 1, forceRefresh = false),
        )

        // The rebuild starts (and finishes) against the now-live epoch 1.
        state.beginBuild(liveEpoch = 1)
        state.onBuildSucceeded(playerHash = "abcd1234", incorporatedEpoch = 1)
        assertTrue(state.canReuseInstance(liveEpoch = 1, forceRefresh = false))
    }

    @Test
    fun `a same-thread forced refresh actually incorporated may claim the newer epoch`() {
        val state = DecipherBuildState()

        state.beginBuild(liveEpoch = 0)
        // forceRefresh inside the build advanced the epoch and the configs were re-extracted,
        // so this build genuinely incorporates epoch 1.
        state.onBuildSucceeded(playerHash = "abcd1234", incorporatedEpoch = 1)

        assertTrue(state.canReuseInstance(liveEpoch = 1, forceRefresh = false))
        assertFalse("a still-newer refresh (epoch 2) is not covered by this build",
            state.canReuseInstance(liveEpoch = 2, forceRefresh = false))
    }

    @Test
    fun `a failed build publishes no usable instance and keeps the next call rebuilding`() {
        val state = DecipherBuildState()

        state.beginBuild(liveEpoch = 0)
        state.onBuildFailed()

        assertFalse(state.instanceUsable)
        assertFalse(state.canReuseInstance(liveEpoch = 0, forceRefresh = false))
        assertNull(state.currentPlayerHash)
        assertFalse(state.buildInProgress)

        // A subsequent successful build recovers normally.
        state.beginBuild(liveEpoch = 0)
        state.onBuildSucceeded(playerHash = "abcd1234", incorporatedEpoch = 0)
        assertTrue(state.canReuseInstance(liveEpoch = 0, forceRefresh = false))
    }

    @Test
    fun `renderer death discards the instance even when the config table never changed`() {
        val state = DecipherBuildState()
        state.beginBuild(liveEpoch = 3)
        state.onBuildSucceeded(playerHash = "abcd1234", incorporatedEpoch = 3)
        assertTrue(state.canReuseInstance(liveEpoch = 3, forceRefresh = false))

        state.onInstanceDiscarded()

        assertFalse("a zombie renderer must never be reused, epoch match notwithstanding",
            state.canReuseInstance(liveEpoch = 3, forceRefresh = false))
        assertFalse(state.instanceUsable)
        assertNull(state.currentPlayerHash)
        assertEquals(-1, state.builtConfigEpoch)
    }

    @Test(expected = IllegalStateException::class)
    fun `publishing a success without a begun build is a programming error`() {
        DecipherBuildState().onBuildSucceeded("abcd1234", incorporatedEpoch = 0)
    }
}
