package io.marvia.android

import android.app.Application
import android.content.Intent
import android.view.View
import androidx.appcompat.app.AppCompatActivity
import io.marvia.android.databinding.ScreenMoreBinding
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config

@RunWith(RobolectricTestRunner::class)
@Config(application = Application::class, sdk = [35])
class UpdateOfferTest {
    @Test fun newerTestReleaseIsVisibleAndOpensTheSellersDownload() {
        val controller = Robolectric.buildActivity(AppCompatActivity::class.java)
        val activity = controller.get()
        activity.setTheme(R.style.Theme_Veil)
        controller.setup()
        try {
            val ui = ScreenMoreBinding.inflate(activity.layoutInflater)
            val screen = MoreScreen(activity, ui, Store(activity),
                { Look.theme(Look.Choice()) }, {}, {}, {}, {}, {}, {}, {}, {}, {})
            val url = "https://panel.example.test/sub/test/app/android"
            screen.showUpdate(TunnelState.Subscription(
                updateVersion = "0.13.0-alpha.6", updateUrl = url))
            assertEquals(View.VISIBLE, ui.rowUpdate.visibility)
            assertEquals(View.VISIBLE, ui.rowUpdateLine.visibility)
            assertTrue(ui.updateTitle.text.toString().contains("0.13.0-alpha.6"))
            ui.rowUpdate.performClick()
            val intent = shadowOf(activity).nextStartedActivity
            assertEquals(Intent.ACTION_VIEW, intent.action)
            assertEquals(url, intent.data.toString())
            screen.showUpdate(TunnelState.Subscription())
            assertEquals(View.GONE, ui.rowUpdate.visibility)
            assertEquals(View.GONE, ui.rowUpdateLine.visibility)
        } finally { controller.pause().stop().destroy() }
    }
}
