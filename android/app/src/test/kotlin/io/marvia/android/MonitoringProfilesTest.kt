package io.marvia.android

import org.junit.Assert.assertEquals
import org.junit.Test

class MonitoringProfilesTest {
    @Test
    fun withoutPowerSavingTheChosenIntervalsStayAsTheyAre() {
        assertEquals(2 to 10, MonitoringProfiles.pace(2, 10, 10, interactive = true, saver = false))
        assertEquals(10 to 10, MonitoringProfiles.pace(2, 10, 10, interactive = false, saver = false))
    }

    @Test
    fun powerSavingSlowsPollingDownToTheBatteryProfile() {
        val battery = MonitoringProfiles.all[0]
        assertEquals(battery.live to battery.ping, MonitoringProfiles.pace(2, 10, 10, interactive = true, saver = true))
        assertEquals(battery.idle to battery.ping, MonitoringProfiles.pace(2, 10, 10, interactive = false, saver = true))
    }

    @Test
    fun powerSavingNeverSpeedsUpWhatIsAlreadySlower() {
        assertEquals(120 to 300, MonitoringProfiles.pace(120, 300, 300, interactive = true, saver = true))
    }

    @Test
    fun turnedOffPingStaysOffUnderPowerSaving() {
        assertEquals(0, MonitoringProfiles.pace(2, 30, 0, interactive = true, saver = true).second)
    }
}
