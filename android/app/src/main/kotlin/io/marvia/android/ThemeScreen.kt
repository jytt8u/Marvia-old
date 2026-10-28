package io.marvia.android

import android.content.ClipData
import android.content.ClipboardManager
import android.graphics.Outline
import android.graphics.drawable.GradientDrawable
import android.view.Gravity
import android.view.LayoutInflater
import android.view.MotionEvent
import android.view.View
import android.view.ViewGroup
import android.view.ViewOutlineProvider
import android.view.inputmethod.EditorInfo
import android.widget.FrameLayout
import android.widget.GridLayout
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.SeekBar
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.res.ResourcesCompat
import androidx.core.graphics.ColorUtils
import androidx.core.view.isVisible
import io.marvia.android.databinding.ItemNodeBinding
import io.marvia.android.databinding.ItemProviderBinding
import io.marvia.android.databinding.ScreenConnectBinding
import io.marvia.android.databinding.ScreenMoreBinding
import io.marvia.android.databinding.ScreenServersBinding
import io.marvia.android.databinding.ScreenThemeBinding
import io.marvia.android.databinding.ViewNavBinding
import kotlin.random.Random

/**
 * ThemeScreen — вид приложения, как в макете: предпросмотр сверху, вкладки
 * Цвет · Свет · Фон · Форма · Ещё, под ними ручки открытой вкладки.
 *
 * Предпросмотр — настоящие экраны в масштабе, а не картинка: главная,
 * серверы и настройки надуваются из тех же разметок и красятся тем же
 * Paint. Что человек крутит внизу, то и видит наверху, и врать предпросмотр
 * не умеет. При смене экрана он переворачивается, как в макете.
 *
 * Ручки те же, что в панели и окне на компьютере, и код темы подходит всем
 * троим: движок один, internal/look. Узор, шрифт и значок — только здесь,
 * в код не входят.
 */
class ThemeScreen(
    private val host: AppCompatActivity,
    private val ui: ScreenThemeBinding,
    private val store: Store,
    /** Открыть системный выбор фото под свой фон. */
    private val onPickBackdrop: () -> Unit,
    /** Открыть выбор картинки под значок в шапке. */
    private val onPickLogo: () -> Unit,
    /** Выбор изменился: перекрасить всё приложение. */
    private val onChanged: () -> Unit,
) {

    private val dp = host.resources.displayMetrics.density
    private val appVersion = host.packageManager.getPackageInfo(host.packageName, 0).versionName.orEmpty()

    private enum class Tab { COLOR, LIGHT, BG, SHAPE, MORE }
    private enum class Screen { MAIN, SETTINGS, SERVERS }

    private var tab = Tab.COLOR

    /** Экран, выбранный руками; null — по вкладке: «форма» показывает настройки, «ещё» — шапку главной со знаком и именем. */
    private var pickedScreen: Screen? = null
    private val previewScreen: Screen
        get() = pickedScreen ?: if (tab == Tab.SHAPE) Screen.SETTINGS else Screen.MAIN

    /** Слот профиля, в который пишет «сохранить сюда». */
    private var slot = 0

    /** Собранный предпросмотр: пересобирается только при смене экрана, иначе перекрашивается. */
    private var stage: View? = null
    private var stageOf: Screen? = null
    private val appearance by lazy { AppearancePicker(host, ui.featuredProfiles, store, onChanged) }

    init {
        ui.customizeTheme.setOnClickListener {
            ui.editorDetails.isVisible = !ui.editorDetails.isVisible
            paint(Look.theme(store.look))
            ui.themeScroll.post {
                val target = if (ui.editorDetails.isVisible) ui.customizeTheme.top else ui.previewCard.top
                val y = (target - 12 * dp).toInt().coerceAtLeast(0)
                if (store.reduceMotion) ui.themeScroll.scrollTo(0, y) else ui.themeScroll.smoothScrollTo(0, y)
            }
        }
        ui.previewExpand.setOnClickListener { openPreview() }
        ui.previewStage.importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO_HIDE_DESCENDANTS
        ui.previewFrame.contentDescription = host.getString(R.string.appearance_preview_example)
        ui.themeReset.setOnClickListener {
            AppearanceProfiles.apply(store, AppearanceProfiles.classic)
            onChanged()
        }
        ui.profileSave.setOnClickListener {
            val list = store.profiles.toMutableList()
            list[slot] = Look.encode(store.look)
            store.profiles = list
            store.saveProfileStyle(slot)
            paint(Look.theme(store.look))
        }
        ui.themeRandom.setOnClickListener { choose(random(), AppearanceStyle.CLASSIC) }
        ui.themeCode.setOnClickListener {
            host.getSystemService(ClipboardManager::class.java)
                ?.setPrimaryClip(ClipData.newPlainText("marvia-look", Look.encode(store.look)))
            Toast.makeText(host, R.string.theme_code_copied, Toast.LENGTH_SHORT).show()
        }
        ui.codeInput.setOnEditorActionListener { v, action, _ ->
            if (action == EditorInfo.IME_ACTION_DONE) {
                applyCode(v.text.toString())
                true
            } else {
                false
            }
        }
        dragPreview(ui.depthSeek, preview = { progress ->
            val choice = store.look.copy(depth = (progress + 8) / 100.0)
            val t = Look.theme(choice)
            paintDepthNote(choice.depth)
            stage?.background = photoBackdrop(t) ?: Backdrop(t)
            ui.lamp.background = Backdrop(Look.theme(choice.copy(kind = if (choice.kind == "flat") "linear" else choice.kind)), Store.PATTERN_NONE)
        }, commit = { progress ->
            val depth = (progress + 8) / 100.0
            if (store.look.depth != depth) choose(store.look.copy(depth = depth))
        })

        ui.bgPick.setOnClickListener { onPickBackdrop() }
        ui.bgFrame.setOnClickListener { editFrame() }
        ui.bgDrop.setOnClickListener {
            store.clearBackdrop()
            onChanged()
        }
        dragPreview(ui.bgDimSeek, preview = { progress ->
            ui.bgDimNote.text = "$progress%"
            stage?.background = photoBackdrop(Look.theme(store.look), progress)
        }, commit = { progress ->
            if (store.backdropDim != progress) {
                store.backdropDim = progress
                onChanged()
            }
        })
        // Сцена — телефон 390×844 в натуральную величину; масштаб и сдвиг от
        // верхнего левого угла ставит zoomTo по вкладке. Рамка обрезает и
        // скругляет углы, как корпус.
        ui.previewStage.pivotX = 0f
        ui.previewStage.pivotY = 0f
        ui.previewFrame.outlineProvider = rounded(18 * dp)
        ui.previewFrame.clipToOutline = true
        zoomTo(animate = false)
    }

    /**
     * Zoom — как показан телефон: размер рамки (dp), масштаб и сдвиг сцены (px).
     *
     * Цвет, свет и фон видны на целом экране в 0,34 — с выбором экрана
     * справа. Форма и «Ещё» приближают то, что меняют: карточки и шапку со
     * знаком. Раньше приближение задавалось числами на глаз, и стоило
     * поменять плотность или выбрать другой экран — в рамке оказывался
     * пустой угол или край кнопки. Теперь рамка наводится на настоящий
     * элемент сцены после раскладки: что бы ни поменялось, он в кадре.
     */
    private data class Zoom(val w: Float, val h: Float, val scale: Float, val dx: Float, val dy: Float)

    /** focusOf — что показывать крупно на этом экране; null — телефон целиком. */
    private fun focusOf(s: View): List<View>? {
        fun v(id: Int): View? = s.findViewById<View>(id)?.takeIf { it.isShown || it.visibility == View.VISIBLE }
        return when {
            tab == Tab.MORE -> listOfNotNull(v(R.id.heroMarkButton), v(R.id.heroName))
            tab != Tab.SHAPE -> null
            previewScreen == Screen.MAIN -> listOfNotNull(v(R.id.todayCard), v(R.id.tilesRow))
            previewScreen == Screen.SETTINGS -> listOfNotNull(v(R.id.rowApps), v(R.id.rowLan))
            else -> listOfNotNull(v(R.id.providerList))
        }?.takeIf { it.isNotEmpty() }
    }

    private fun zoomFor(t: Tab): Zoom {
        val whole = Zoom(172f, 372f, 0.44f, 0f, 0f)
        if (!ui.editorDetails.isVisible || (t != Tab.SHAPE && t != Tab.MORE)) return whole
        val s = stage as? ViewGroup ?: return whole
        val targets = focusOf(s) ?: return whole
        if (s.width == 0) return whole
        // Прямоугольник целей в координатах сцены — без её масштаба.
        val box = android.graphics.Rect()
        val r = android.graphics.Rect()
        for (v in targets) {
            r.set(0, 0, v.width, v.height)
            s.offsetDescendantRectToMyCoords(v, r)
            if (box.isEmpty) box.set(r) else box.union(r)
        }
        if (box.isEmpty) return whole
        // Во всю ширину карточки предпросмотра: рамка уже неё оставляла
        // справа пустую полосу, а место под крупный кадр и так дорого.
        val row = ui.previewFrame.parent as? View
        val w = row?.let { (it.width - it.paddingLeft - it.paddingRight) / dp }?.takeIf { it > 200f } ?: 302f
        val h = 264f
        val pad = 12 * dp
        // По ширине, а не целиком: вписанный по высоте столбик карточек
        // выходил узким, с пустыми полями по бокам, и формы было не
        // разглядеть. Низ цели может уйти за край — важен её верх и то, как
        // выглядят карточки, а не сколько их. Шапке хватает натуральной
        // величины: крупнее знак зернится.
        val scale = ((w * dp - 2 * pad) / box.width()).coerceIn(0.34f, 1f)
        val dx = (w * dp - box.width() * scale) / 2 - box.left * scale
        val dy = if (box.height() * scale <= h * dp - 2 * pad) {
            (h * dp - box.height() * scale) / 2 - box.top * scale
        } else {
            pad - box.top * scale
        }
        // Край сцены в рамке не показываем никогда: из-под него видна
        // подложка, и на живом телефоне это выглядело серым прямоугольником
        // поверх экрана. Шапка у верхнего края так и встаёт к верху.
        val sw = s.width * scale
        val sh = s.height * scale
        val cx = dx.coerceIn(minOf(0f, w * dp - sw), 0f)
        val cy = dy.coerceIn(minOf(0f, h * dp - sh), 0f)
        return Zoom(w, h, scale, cx / dp, cy / dp)
    }

    private var zoomAnim: android.animation.ValueAnimator? = null

    /**
     * Пока палец на ручке, меняется только свет в предпросмотре. Полная
     * пересборка карточек и запись настроек происходят один раз при отпускании.
     * Родительская прокрутка не перехватывает горизонтальное перетаскивание.
     */
    private fun dragPreview(bar: SeekBar, preview: (Int) -> Unit, commit: (Int) -> Unit) {
        var tracking = false
        var queued = false
        var lastFrame = 0L
        val redraw = Runnable {
            queued = false
            lastFrame = android.os.SystemClock.uptimeMillis()
            preview(bar.progress)
        }
        bar.minimumHeight = (48 * dp).toInt()
        bar.setOnTouchListener { view, event ->
            when (event.actionMasked) {
                MotionEvent.ACTION_DOWN -> view.parent.requestDisallowInterceptTouchEvent(true)
                MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> view.parent.requestDisallowInterceptTouchEvent(false)
            }
            false
        }
        bar.setOnSeekBarChangeListener(object : SeekBar.OnSeekBarChangeListener {
            override fun onStartTrackingTouch(seek: SeekBar) { tracking = true }
            override fun onProgressChanged(seek: SeekBar, progress: Int, fromUser: Boolean) {
                if (!fromUser) return
                if (!tracking) { commit(progress); return }
                if (!queued) {
                    queued = true
                    // Большой предпросмотр пересоздаёт градиент при каждом
                    // изменении. 30 кадров/с достаточно для перетаскивания,
                    // а освобождённое время остаётся самому ползунку.
                    val wait = (33L - (android.os.SystemClock.uptimeMillis() - lastFrame)).coerceAtLeast(0L)
                    bar.postDelayed(redraw, wait)
                }
            }
            override fun onStopTrackingTouch(seek: SeekBar) {
                tracking = false
                bar.removeCallbacks(redraw)
                queued = false
                // Последний кадр мог быть в очереди. Доводим предпросмотр
                // до значения под пальцем даже если оно вернулось к исходному.
                preview(seek.progress)
                commit(seek.progress)
            }
        })
    }

    private fun zoomTo(animate: Boolean) {
        val z = zoomFor(tab)
        val frame = ui.previewFrame
        val stage = ui.previewStage
        val lp = frame.layoutParams
        val fromW = if (lp.width > 0) lp.width.toFloat() else z.w * dp
        val fromH = if (lp.height > 0) lp.height.toFloat() else z.h * dp
        val fromS = stage.scaleX
        val fromX = stage.translationX
        val fromY = stage.translationY
        val same = kotlin.math.abs(fromS - z.scale) < 0.001f &&
            kotlin.math.abs(fromX - z.dx * dp) < 0.5f && kotlin.math.abs(fromY - z.dy * dp) < 0.5f &&
            kotlin.math.abs(fromW - z.w * dp) < 0.5f && kotlin.math.abs(fromH - z.h * dp) < 0.5f
        if (same) return
        zoomAnim?.cancel()
        val apply = { f: Float ->
            lp.width = (fromW + (z.w * dp - fromW) * f).toInt()
            lp.height = (fromH + (z.h * dp - fromH) * f).toInt()
            frame.layoutParams = lp
            stage.scaleX = fromS + (z.scale - fromS) * f
            stage.scaleY = stage.scaleX
            stage.translationX = fromX + (z.dx * dp - fromX) * f
            stage.translationY = fromY + (z.dy * dp - fromY) * f
        }
        if (!animate || store.reduceMotion) { apply(1f); return }
        zoomAnim = android.animation.ValueAnimator.ofFloat(0f, 1f).apply {
            duration = 550
            // Кривая макета, cubic-bezier(.2,.8,.2,1): быстрый разгон и
            // долгое мягкое торможение.
            interpolator = android.view.animation.PathInterpolator(0.2f, 0.8f, 0.2f, 1f)
            addUpdateListener { apply(it.animatedValue as Float) }
            start()
        }
    }

    private fun choose(next: Look.Choice, style: AppearanceStyle = store.appearanceStyle) {
        store.look = Look.normalize(next)
        store.appearanceStyle = style
        onChanged()
    }

    /** applyCode применяет код темы; чужой — говорит об этом, а не красит в серый. */
    private fun applyCode(text: String) {
        val next = Look.decode(text)
        ui.codeBad.isVisible = next == null
        if (next == null) return
        ui.codeInput.setText("")
        choose(next, AppearanceStyle.CLASSIC)
    }

    private fun random(): Look.Choice {
        val r = Random.Default
        fun <T> pick(list: List<T>): T = list[r.nextInt(list.size)]
        return Look.Choice(
            preset = pick(Look.presets).key,
            accent = pick(listOf(0) + LookTable.accents),
            kind = pick(listOf("linear", "radial", "aurora")),
            dir = pick(LookTable.dirs).key,
            depth = (20 + r.nextInt(76)) / 100.0,
            tint = pick(listOf(0, 0) + LookTable.accents),
            radius = pick(LookTable.radii).key,
            density = pick(LookTable.densities).key,
            btn = pick(LookTable.buttons),
            glow = pick(LookTable.glows).key,
            card = pick(LookTable.cards),
        )
    }

    /** paint перерисовывает выбор под текущую тему. Зовётся при каждой смене. */
    fun paint(t: Theme, flip: Boolean = false) {
        val choice = store.look
        ui.featuredSection.isVisible = true
        appearance.render(t)
        ui.selectedAppearance.text = AppearanceProfiles.selected(store)?.let {
            host.getString(it.name) + " · " + host.getString(it.description)
        } ?: host.getString(R.string.theme_look_custom)
        ui.customizeTheme.setText(if (ui.editorDetails.isVisible) R.string.appearance_customizing else R.string.appearance_customize)
        ui.previewExpand.background = Paint.rounded(t.surf2, minOf(t.r, 12), dp)
        ui.previewExpand.setTextColor(t.fg)
        // Ряды собираются заново, и прокрутка на миг теряет опору — возвращаем
        // её после раскладки, иначе каждое нажатие уносит экран наверх.
        val scrollY = ui.themeScroll.scrollY
        ui.themeScroll.post { ui.themeScroll.scrollTo(0, scrollY) }
        paintPreview(t, flip)
        paintTabs(t)
        // Кадр — после раскладки: плотность и скругления двигают карточки, и
        // рамка должна ехать за ними, а не смотреть в прежнее место.
        ui.previewStage.post { zoomTo(animate = true) }

        ui.tabColor.isVisible = tab == Tab.COLOR
        ui.tabLight.isVisible = tab == Tab.LIGHT
        ui.tabBg.isVisible = tab == Tab.BG
        ui.tabShape.isVisible = tab == Tab.SHAPE
        ui.tabMore.isVisible = tab == Tab.MORE

        when (tab) {
            Tab.COLOR -> {
                paintLooks(t, choice)
                ui.lookName.text = LookTable.looks.firstOrNull { Look.same(choice, it) }?.let { lookName(it) } ?: host.getString(R.string.theme_look_custom)
                paintSwatches(ui.accentStrip, t, LookTable.accents, t.acc.takeIf { choice.accent != 0 } ?: 0, big = true) { choose(choice.copy(accent = it)) }
                paintSwatches(ui.tintStrip, t, listOf(0) + LookTable.accents, choice.tint, big = false) { choose(choice.copy(tint = it)) }
                ui.tintName.text = if (choice.tint == 0) host.getString(R.string.theme_tint_auto) else ""
            }
            Tab.LIGHT -> paintLight(t, choice)
            Tab.BG -> {
                paintPatterns(t)
                paintBackdrop(t)
            }
            Tab.SHAPE -> {
                paintPills(ui.cardRow, t, LookTable.cards, choice.card, ::cardName) { choose(choice.copy(card = it)) }
                paintPills(ui.radiusRow, t, LookTable.radii.map { it.key }, choice.radius, ::radiusName) { choose(choice.copy(radius = it)) }
                paintPills(ui.densityRow, t, LookTable.densities.map { it.key }, choice.density, ::densityName) { choose(choice.copy(density = it)) }
                paintPills(ui.buttonRow, t, LookTable.buttons, choice.btn, ::btnName) { choose(choice.copy(btn = it)) }
                paintPills(ui.glowRow, t, LookTable.glows.map { it.key }, choice.glow, ::glowName) { choose(choice.copy(glow = it)) }
            }
            Tab.MORE -> {
                paintPills(ui.homeRow, t, HomeLayout.entries.map { it.key }, store.homeView, ::homeName) {
                    store.homeView = it
                    onChanged()
                }
                paintLogos(t)
                paintFonts(t)
                paintProfiles(t)
                ui.themeCode.text = Look.encode(choice)
                ui.themeCode.background = GradientDrawable().apply {
                    cornerRadius = minOf(t.r, 12) * dp
                    setColor(t.shade)
                    setStroke((1 * dp).toInt(), t.line)
                }
                ui.codeInput.background = GradientDrawable().apply {
                    cornerRadius = minOf(t.r, 12) * dp
                    setColor(t.shade)
                    setStroke((1 * dp).toInt(), t.line)
                }
                ui.themeReset.background = Paint.rounded(t.surf2, minOf(t.r, 12), dp)
                ui.themeReset.setTextColor(t.dim)
            }
        }
    }

    // ---------------------------------------------------------- вкладки

    private fun paintTabs(t: Theme) {
        val row = ui.tabsRow
        row.removeAllViews()
        val tabs = listOf(
            Triple(Tab.COLOR, R.string.theme_tab_color, R.drawable.ic_tab_color),
            Triple(Tab.LIGHT, R.string.theme_tab_light, R.drawable.ic_tab_light),
            Triple(Tab.BG, R.string.theme_tab_bg, R.drawable.ic_tab_bg),
            Triple(Tab.SHAPE, R.string.theme_tab_shape, R.drawable.ic_tab_shape),
            Triple(Tab.MORE, R.string.theme_tab_more, R.drawable.ic_tab_more),
        )
        for ((key, name, icon) in tabs) {
            val on = tab == key
            val col = LinearLayout(host).apply {
                orientation = LinearLayout.VERTICAL
                gravity = Gravity.CENTER_HORIZONTAL
                setPadding(0, (4 * dp).toInt(), 0, (4 * dp).toInt())
                isClickable = true
                isFocusable = true
                setOnClickListener {
                    if (tab != key) {
                        val before = previewScreen
                        tab = key
                        // Экран, выбранный руками, — для этой вкладки. На
                        // «Ещё» он оставался «Настройками», и смена знака
                        // шла мимо кадра: шапки со знаком там нет.
                        pickedScreen = null
                        paint(t, flip = previewScreen != before)
                    }
                }
            }
            val pill = FrameLayout(host).apply {
                background = if (on) Paint.rounded(t.accSoft, 999, dp) else null
                addView(ImageView(host).apply {
                    setImageResource(icon)
                    setColorFilter(if (on) t.fg else t.dim)
                }, FrameLayout.LayoutParams((20 * dp).toInt(), (20 * dp).toInt(), Gravity.CENTER))
            }
            col.addView(pill, LinearLayout.LayoutParams((44 * dp).toInt(), (28 * dp).toInt()))
            col.addView(TextView(host).apply {
                text = host.getString(name)
                // По центру: без этого строка брала всю ширину колонки и
                // прижималась влево, уезжая из-под значка.
                gravity = Gravity.CENTER
                maxLines = 1
                textSize = 10.5f
                setTextColor(if (on) t.fg else t.dim)
                typeface = android.graphics.Typeface.create(typeface, if (on) android.graphics.Typeface.BOLD else android.graphics.Typeface.NORMAL)
                setPadding(0, (3 * dp).toInt(), 0, 0)
            })
            row.addView(col, LinearLayout.LayoutParams(0, WRAP, 1f))
        }
        ui.tabHint.text = host.getString(
            when (tab) {
                Tab.COLOR -> R.string.theme_hint_color
                Tab.LIGHT -> R.string.theme_hint_light
                Tab.BG -> R.string.theme_hint_bg
                Tab.SHAPE -> R.string.theme_hint_shape
                Tab.MORE -> R.string.theme_hint_more
            },
        )
    }

    // ------------------------------------------------------ предпросмотр

    /**
     * paintPreview собирает экран в сцену и красит его. Экран тот же, что
     * настоящий: разметка та же, данные — примерные, чтобы было что показать.
     */
    private fun paintPreview(t: Theme, flip: Boolean) {
        if (stageOf != previewScreen || stage == null) {
            ui.previewStage.removeAllViews()
            val built = buildStage(previewScreen)
            ui.previewStage.addView(built, FrameLayout.LayoutParams(MATCH, MATCH))
            stage = built
            stageOf = previewScreen
        }
        val s = stage ?: return
        if (previewScreen == Screen.MAIN) {
            val connect = s.findViewById<View>(R.id.connectRoot)
            ConnectionAppearance.apply(ScreenConnectBinding.bind(connect), store.appearanceStyle, t)
        }
        Paint.apply(s, t)
        s.background = photoBackdrop(t) ?: Backdrop(t)
        ui.previewFrame.foreground = GradientDrawable().apply { cornerRadius = 18 * dp; setColor(0); setStroke((1 * dp).toInt(), t.line) }
        ui.previewFrame.elevation = 8 * dp
        fillStage(s, t)
        ui.previewFrame.animate().cancel()
        if (flip && !store.reduceMotion) {
            // Переворот, как в макете: карта уходит ребром и возвращается новой.
            ui.previewFrame.cameraDistance = 8000 * dp
            ui.previewFrame.rotationY = -90f
            ui.previewFrame.alpha = 0.3f
            ui.previewFrame.animate().rotationY(0f).alpha(1f).setDuration(700).setInterpolator(android.view.animation.DecelerateInterpolator(2f)).start()
        } else {
            ui.previewFrame.rotationY = 0f
            ui.previewFrame.alpha = 1f
        }

        // Переключатель экрана предпросмотра: точка и имя. Столбиком справа,
        // а в приближении, где справа места нет, — строкой под телефоном.
        val zoomed = (ui.editorDetails.isVisible && (tab == Tab.SHAPE || tab == Tab.MORE)) || host.resources.configuration.fontScale > 1.5f
        val picker = if (zoomed) ui.previewPickerBelow else ui.previewPicker
        ui.previewPicker.isVisible = !zoomed
        ui.previewPickerBelow.isVisible = zoomed
        ui.previewPickerBelow.orientation = if (host.resources.configuration.fontScale > 1.5f) LinearLayout.VERTICAL else LinearLayout.HORIZONTAL
        picker.removeAllViews()
        for ((key, name) in listOf(Screen.MAIN to R.string.theme_pv_main, Screen.SETTINGS to R.string.theme_pv_settings, Screen.SERVERS to R.string.theme_pv_servers)) {
            val on = previewScreen == key
            val item = LinearLayout(host).apply {
                orientation = LinearLayout.HORIZONTAL
                gravity = Gravity.CENTER_VERTICAL
                setPadding((10 * dp).toInt(), (8 * dp).toInt(), (10 * dp).toInt(), (8 * dp).toInt())
                isClickable = true
                isFocusable = true
                minimumHeight = (48 * dp).toInt()
                setOnClickListener {
                    if (previewScreen != key) {
                        pickedScreen = key
                        paintPreview(t, flip = true)
                        ui.previewStage.post { zoomTo(animate = true) }
                    }
                }
            }
            item.addView(View(host).apply { background = Paint.circle(if (on) t.acc else t.line) }, LinearLayout.LayoutParams((8 * dp).toInt(), (8 * dp).toInt()).apply { marginEnd = (8 * dp).toInt() })
            item.addView(TextView(host).apply {
                text = host.getString(name)
                textSize = 13f
                setTextColor(if (on) t.fg else t.dim)
                typeface = android.graphics.Typeface.create(typeface, if (on) android.graphics.Typeface.BOLD else android.graphics.Typeface.NORMAL)
            })
            picker.addView(item)
        }
    }

    /** buildStage надувает экран предпросмотра: тот же XML, что у настоящего. */
    private fun openPreview() {
        val t = Look.theme(store.look)
        val dialog = android.app.Dialog(host, R.style.Theme_Veil)
        val column = LinearLayout(host).apply {
            orientation = LinearLayout.VERTICAL
            setPadding((16 * dp).toInt(), (24 * dp).toInt(), (16 * dp).toInt(), (16 * dp).toInt())
        }
        column.addView(TextView(host).apply {
            setText(R.string.appearance_preview_example)
            textSize = 16f
            tag = "fg"
        })
        val screen = buildStage(previewScreen)
        column.addView(screen, LinearLayout.LayoutParams(MATCH, 0, 1f))
        fillStage(screen, t)
        screen.background = photoBackdrop(t) ?: Backdrop(t)
        setTouchless(screen)
        column.addView(TextView(host).apply {
            setText(R.string.appearance_close_preview)
            textSize = 16f
            gravity = Gravity.CENTER
            minimumHeight = (52 * dp).toInt()
            tag = "action"
            isFocusable = true
            setOnClickListener { dialog.dismiss() }
        }, LinearLayout.LayoutParams(MATCH, WRAP))
        Paint.apply(column, t)
        dialog.setContentView(column)
        dialog.show()
        dialog.window?.apply {
            setBackgroundDrawable(android.graphics.drawable.ColorDrawable(t.bg))
            setLayout(MATCH, MATCH)
        }
    }

    private fun buildStage(which: Screen): View {
        val inflater = LayoutInflater.from(host)
        val column = LinearLayout(host).apply { orientation = LinearLayout.VERTICAL }
        val body: View = when (which) {
            Screen.MAIN -> ScreenConnectBinding.inflate(inflater, column, false).root
            Screen.SETTINGS -> ScreenMoreBinding.inflate(inflater, column, false).root
            Screen.SERVERS -> ScreenServersBinding.inflate(inflater, column, false).root
        }
        column.addView(body, LinearLayout.LayoutParams(MATCH, 0, 1f))
        val nav = ViewNavBinding.inflate(inflater, column, false)
        column.addView(nav.root)
        // Сцена — не кнопки: нажатия по ней ничего не делают.
        setTouchless(column)
        return column
    }

    private fun setTouchless(v: View) {
        v.isClickable = false
        v.isFocusable = false
        if (v is ViewGroup) for (i in 0 until v.childCount) setTouchless(v.getChildAt(i))
    }

    /** fillStage — примерные данные: подключено, Финляндия, расход за день. */
    private fun fillStage(s: View, t: Theme) {
        fun <T : View> id(id: Int): T? = s.findViewById(id)
        when (previewScreen) {
            Screen.MAIN -> {
                id<PowerButton>(R.id.powerAction)?.apply { motionEnabled = false; theme = t; state = PowerButton.State.ON }
                id<TextView>(R.id.connectionBadge)?.apply {
                    setText(R.string.connect_badge_on); setTextColor(t.acc)
                }
                id<View>(R.id.todayCard)?.isVisible = store.homeStats
                id<View>(R.id.tilesRow)?.isVisible = store.homeStats
                id<TextView>(R.id.powerHint)?.isVisible = false
                id<HaloView>(R.id.halo)?.apply { theme = t; lit = true; isVisible = id<PowerButton>(R.id.powerAction)?.dottedPower != true }
                id<TextView>(R.id.statusText)?.apply { setText(R.string.status_on); setTextColor(if (t.dark) t.fg else t.acc) }
                id<TextView>(R.id.nodeLine)?.text = host.getString(R.string.theme_preview_country) + " · " + host.getString(R.string.theme_preview_place)
                // Вид главной в предпросмотре — с теми же блоками, что на настоящей:
                // иначе «Сцена» и «Marvia» здесь выглядели бы одинаково.
                val home = HomeLayout.of(store.homeView)
                val b = ScreenConnectBinding.bind(id<View>(R.id.connectRoot)!!)
                b.nodeLine.isVisible = home == HomeLayout.MARVIA
                b.placeCard.isVisible = home.cards
                if (home.cards) HomeBlocks.place(b, HomeBlocks.Place(b.nodeLine.text.toString() + " · fi-1", 38, live = true, manual = false), t)
                HomeBlocks.plan(b, HomeBlocks.Plan("Marvia", java.time.Instant.now().plusSeconds(19L * 86400).toString(), 50L shl 30, 32L shl 30, ""), t, home)
                id<TextView>(R.id.todayTotal)?.text = Format.size(host, 4_509_715_660L)
                id<HourBars>(R.id.todayBars)?.apply { theme = t; hours = MINI.map { it.toLong() }; current = 19 }
                id<TextView>(R.id.downValue)?.text = host.getString(R.string.stats_mbps, "48,0")
                id<TextView>(R.id.upValue)?.text = host.getString(R.string.stats_mbps, "6,2")
                id<SpeedSpark>(R.id.downSpark)?.apply { theme = t; show(SPARK_DOWN) }
                id<SpeedSpark>(R.id.upSpark)?.apply { theme = t; show(SPARK_UP) }
                // Шапка — как настоящая: знак по выбору из «Ещё», имя шрифтом темы.
                // Иначе вкладка «Ещё» меняла бы то, чего в предпросмотре не видно.
                val custom = if (store.logo == Store.LOGO_CUSTOM) store.logoBitmap() else null
                id<View>(R.id.heroName)?.isVisible = tab == Tab.MORE
                ConnectionAppearance.header(ScreenConnectBinding.bind(id<View>(R.id.connectRoot)!!), store.appearanceStyle)
                id<View>(R.id.heroMarkButton)?.isVisible = store.logo != Store.LOGO_NONE
                id<View>(R.id.heroMark)?.isVisible = store.logo == Store.LOGO_MARVIA || (store.logo == Store.LOGO_CUSTOM && custom == null)
                id<ImageView>(R.id.heroCustom)?.apply {
                    isVisible = custom != null
                    if (custom != null) { setImageBitmap(custom); clipToOutline = true; outlineProvider = rounded(9 * dp) }
                }
            }
            Screen.SERVERS -> {
                id<TextView>(R.id.serversSubtitle)?.text = host.resources.getQuantityString(R.plurals.servers_providers, 2, 2) + " · " + host.resources.getQuantityString(R.plurals.servers_count, 8, 8)
                id<TextView>(R.id.autoNote)?.text = host.getString(R.string.servers_auto_fastest, host.getString(R.string.theme_preview_country) + " · fi-1")
                id<View>(R.id.autoRow)?.background = Paint.card(t, dp, stroke = t.acc).apply { setStroke((1.5f * dp).toInt(), t.acc) }
                id<View>(R.id.autoIconBox)?.background = Paint.rounded(t.accSoft, 12, dp)
                id<View>(R.id.autoMark)?.background = Paint.circle(t.acc)
                id<ImageView>(R.id.autoMarkCheck)?.setColorFilter(t.accFg)
                id<View>(R.id.refreshAll)?.background = Paint.card(t, dp)
                val list = id<LinearLayout>(R.id.providerList) ?: return
                list.removeAllViews()
                val inflater = LayoutInflater.from(host)
                val p = ItemProviderBinding.inflate(inflater, list, false)
                p.providerName.text = host.getString(R.string.theme_preview_provider)
                p.providerMeta.text = host.resources.getQuantityString(R.plurals.servers_count, 6, 6) + " · " + host.getString(R.string.servers_sub_active)
                p.providerChevron.rotation = 90f
                p.providerAge.text = host.getString(R.string.servers_age_hours, 1)
                p.providerDays.text = host.getString(R.string.servers_days_left, host.resources.getQuantityString(R.plurals.days_left, 24, 24))
                p.providerLeft.text = host.getString(R.string.servers_quota, "41", Format.size(host, 100L shl 30))
                for (b in listOf(p.providerRefresh, p.providerMeasure, p.providerMore)) b.background = Paint.circle(t.surf2)
                p.providerTrack.background = Paint.rounded(t.shade, 999, dp)
                p.providerTrack.clipToOutline = true
                p.providerFill.background = Paint.rounded(t.acc, 999, dp)
                p.providerTrack.post { p.providerFill.layoutParams = p.providerFill.layoutParams.apply { width = (p.providerTrack.width * 0.41f).toInt() } }
                val rows = listOf(
                    Triple(host.getString(R.string.theme_preview_country), "Helsinki · fi-1 · " + host.getString(R.string.node_traffic_now), 33),
                    Triple("Нидерланды", "Amsterdam · nl-1", 41),
                )
                rows.forEachIndexed { i, (name, note, ms) ->
                    val n = ItemNodeBinding.inflate(inflater, p.providerNodes, false)
                    n.nodeFlag.text = Flags.of(name)
                    n.nodeTitle.text = name
                    n.nodeTitle.setTextColor(t.fg)
                    n.nodeNote.text = note
                    n.nodeBars.lit = SignalBars.of(ms.toLong(), true); n.nodeBars.on = t.acc; n.nodeBars.off = t.line
                    n.nodePing.text = host.getString(R.string.node_ms, ms)
                    n.nodePing.setTextColor(if (i == 0) t.fg else pingColor(t, ms.toLong()))
                    if (i == 0) n.root.setBackgroundColor(ColorUtils.setAlphaComponent(t.acc, 26))
                    p.providerNodes.addView(n.root)
                }
                p.root.tag = null
                list.addView(p.root)
                Paint.apply(list, t)
                p.root.background = Paint.card(t, dp)
                id<View>(R.id.serversEmpty)?.isVisible = false
            }
            Screen.SETTINGS -> {
                id<TextView>(R.id.moreTitle)?.setText(R.string.more_title)
                id<TextView>(R.id.moreTitle)?.textSize = if (store.appearanceStyle == AppearanceStyle.SIGNAL) 32f else 26f
                id<TextView>(R.id.moreSub)?.text = host.getString(R.string.more_sub, appVersion)
                id<TextView>(R.id.appsSummary)?.text = host.getString(R.string.apps_summary_names, 3, "Госуслуги, Сбербанк, Т-Банк")
                id<TextView>(R.id.languageValue)?.setText(LanguageScreen.label(store.language))
                id<TextView>(R.id.dnsValue)?.text = "Cloudflare"
                id<TextView>(R.id.aboutSub)?.text = host.getString(R.string.about_sub, appVersion)
                id<LinearLayout>(R.id.appearanceRows)?.let { rows ->
                    AppearanceSettings(host, rows, store, { t }, {}).render()
                }
                id<com.google.android.material.materialswitch.MaterialSwitch>(R.id.switchAutostart)?.isChecked = true
                id<com.google.android.material.materialswitch.MaterialSwitch>(R.id.switchDnsSecure)?.isChecked = true
                id<View>(R.id.moreBack)?.isVisible = false
            }
        }
        // Нижняя панель сцены: таблетка под активной вкладкой, как настоящая.
        val active = when (previewScreen) { Screen.MAIN -> R.id.navConnectPill to R.id.navConnectLabel; Screen.SERVERS -> R.id.navServersPill to R.id.navServersLabel; Screen.SETTINGS -> R.id.navMorePill to R.id.navMoreLabel }
        for ((pill, icon, label) in listOf(
            Triple(R.id.navConnectPill, R.id.navConnectIcon, R.id.navConnectLabel),
            Triple(R.id.navServersPill, R.id.navServersIcon, R.id.navServersLabel),
            Triple(R.id.navStatsPill, R.id.navStatsIcon, R.id.navStatsLabel),
            Triple(R.id.navThemePill, R.id.navThemeIcon, R.id.navThemeLabel),
            Triple(R.id.navMorePill, R.id.navMoreIcon, R.id.navMoreLabel),
        )) {
            Paint.navigationTab(id<View>(pill) ?: continue, id<ImageView>(icon) ?: continue,
                id<TextView>(label) ?: continue, pill == active.first, t)
        }
        setTouchless(s)
    }

    // ---------------------------------------------------------------- виды

    /** paintLooks — лента готовых видов: квадрат фона вида с шариком акцента и имя. */
    /** Карточки готовых видов, собранные один раз: обводка и подпись перекрашиваются на месте. */
    private var lookCards: List<Pair<FrameLayout, TextView>> = emptyList()

    /**
     * paintLooks — ряд готовых видов. Карточек три десятка, и раньше каждое
     * нажатие пересобирало их все с нуля вместе с фонами: заметная пауза на
     * слабом телефоне и сброс прокрутки ряда. Теперь собираются раз, а при
     * выборе меняются только обводка и подпись.
     */
    private fun paintLooks(t: Theme, choice: Look.Choice) {
        val strip = ui.looksStrip
        if (lookCards.size == LookTable.looks.size) {
            for ((i, lk) in LookTable.looks.withIndex()) {
                val (canvas, label) = lookCards[i]
                val on = Look.same(choice, lk)
                val preview = Look.theme(Look.ofLook(lk))
                (canvas.foreground as? GradientDrawable)?.apply {
                    cornerRadius = minOf(t.r + 6, 24) * dp
                    setStroke(((if (on) 2.5f else 1f) * dp).toInt(), if (on) t.acc else preview.line)
                }
                canvas.outlineProvider = rounded(minOf(t.r + 6, 24) * dp)
                label.setTextColor(if (on) t.fg else t.dim)
                label.typeface = android.graphics.Typeface.create(label.typeface, if (on) android.graphics.Typeface.BOLD else android.graphics.Typeface.NORMAL)
            }
            return
        }
        strip.removeAllViews()
        val cards = ArrayList<Pair<FrameLayout, TextView>>(LookTable.looks.size)
        for ((i, lk) in LookTable.looks.withIndex()) {
            val preview = Look.theme(Look.ofLook(lk))
            val on = Look.same(choice, lk)
            val col = LinearLayout(host).apply {
                orientation = LinearLayout.VERTICAL
                gravity = Gravity.CENTER_HORIZONTAL
                isClickable = true
                isFocusable = true
                setOnClickListener { choose(Look.ofLook(lk), AppearanceStyle.CLASSIC) }
            }
            val canvas = FrameLayout(host).apply {
                background = Backdrop(preview, Store.PATTERN_NONE)
                outlineProvider = rounded(minOf(t.r + 6, 24) * dp)
                clipToOutline = true
                foreground = GradientDrawable().apply {
                    cornerRadius = minOf(t.r + 6, 24) * dp
                    setColor(0)
                    setStroke(((if (on) 2.5f else 1f) * dp).toInt(), if (on) t.acc else preview.line)
                }
                addView(View(host).apply {
                    background = GradientDrawable().apply {
                        shape = GradientDrawable.OVAL
                        gradientType = GradientDrawable.RADIAL_GRADIENT
                        gradientRadius = 20 * dp
                        setGradientCenter(0.35f, 0.3f)
                        setColors(intArrayOf(Look.mix(preview.acc, 0xFFFFFFFF.toInt(), 0.35), preview.acc))
                    }
                    elevation = 6 * dp
                }, FrameLayout.LayoutParams((30 * dp).toInt(), (30 * dp).toInt(), Gravity.CENTER))
            }
            col.addView(canvas, LinearLayout.LayoutParams((84 * dp).toInt(), (84 * dp).toInt()))
            val label = TextView(host).apply {
                text = lookName(lk)
                textSize = 11f
                maxLines = 1
                gravity = Gravity.CENTER
                setTextColor(if (on) t.fg else t.dim)
                typeface = android.graphics.Typeface.create(typeface, if (on) android.graphics.Typeface.BOLD else android.graphics.Typeface.NORMAL)
                setPadding(0, (6 * dp).toInt(), 0, 0)
            }
            col.addView(label, LinearLayout.LayoutParams((84 * dp).toInt(), WRAP))
            strip.addView(col, LinearLayout.LayoutParams(WRAP, WRAP).apply { if (i > 0) marginStart = (10 * dp).toInt() })
            cards.add(canvas to label)
        }
        lookCards = cards
    }

    /**
     * lookName — название вида: по ключу пресета из строк приложения. Пресет
     * встречается в видах дважды (серый мягкий и серый ровный) — второму
     * добавляется характер света, чтобы карточки не назывались одинаково.
     */
    private fun lookName(lk: LookTable.Look): String {
        val name = named("look_", lk.preset)
        val twice = LookTable.looks.count { it.preset == lk.preset } > 1
        return if (twice && lk != LookTable.looks.first { it.preset == lk.preset }) name + " · " + kindName(lk.kind) else name
    }

    /** named — строка по ключу таблицы; без строки покажет ключ, а тест в internal/look до этого не допустит. */
    private fun named(prefix: String, key: String): String {
        val id = host.resources.getIdentifier(prefix + key, "string", host.packageName)
        return if (id == 0) key else host.getString(id)
    }

    private fun homeName(key: String) = named("home_view_", key)
    private fun densityName(key: String) = named("density_", key)
    private fun radiusName(key: String) = named("radius_", key)
    private fun cardName(key: String) = named("card_", key)
    private fun btnName(key: String) = named("btn_", key)
    private fun glowName(key: String) = named("glow_", key)
    private fun kindName(key: String) = named("kind_", key)
    private fun dirName(key: String) = named("dir_", key)

    // ---------------------------------------------------------------- цвет

    /** paintSwatches — лента кружков; ноль — «как в пресете», кружок с градиентом акцента. */
    private fun paintSwatches(strip: LinearLayout, t: Theme, colors: List<Int>, current: Int, big: Boolean, onPick: (Int) -> Unit) {
        strip.removeAllViews()
        val size = ((if (big) 40 else 32) * dp).toInt()
        for ((i, color) in colors.withIndex()) {
            val on = color == current
            val swatch = View(host).apply {
                background = GradientDrawable().apply {
                    shape = GradientDrawable.OVAL
                    if (color == 0) {
                        orientation = GradientDrawable.Orientation.TL_BR
                        setColors(intArrayOf(t.acc, Look.mix(t.acc, 0xFF000000.toInt(), 0.6)))
                    } else {
                        setColor(color)
                    }
                    // Выбранный — кольцом своего цвета через зазор цвета фона: обводка тем же цветом была бы невидима.
                    if (on) setStroke((3 * dp).toInt(), t.shade)
                }
                foreground = if (on) GradientDrawable().apply {
                    shape = GradientDrawable.OVAL
                    setColor(0)
                    setStroke((2 * dp).toInt(), if (color == 0) t.acc else color)
                } else null
                isClickable = true
                isFocusable = true
                setOnClickListener { onPick(color) }
            }
            strip.addView(swatch, LinearLayout.LayoutParams(size, size).apply { if (i > 0) marginStart = (10 * dp).toInt() })
        }
    }

    // ---------------------------------------------------------------- свет

    private fun paintLight(t: Theme, choice: Look.Choice) {
        // Лампа: экран, залитый настоящим фоном; нажатие в угол переносит свет.
        // Ровный фон света не показывает — лампа рисует мягкий, чтобы было видно, куда нажимать.
        val litKind = if (choice.kind == "flat") "linear" else choice.kind
        ui.lamp.background = Backdrop(Look.theme(choice.copy(kind = litKind)), Store.PATTERN_NONE)
        ui.lamp.outlineProvider = rounded(t.r * dp)
        ui.lamp.clipToOutline = true
        ui.lamp.foreground = GradientDrawable().apply {
            cornerRadius = t.r * dp
            setColor(0)
            setStroke((1 * dp).toInt(), t.line)
        }

        val zones = ui.lampZones
        zones.removeAllViews()
        for (key in LAMP) {
            val on = choice.dir == key && choice.kind != "flat"
            val sun = View(host).apply {
                background = GradientDrawable().apply {
                    shape = GradientDrawable.OVAL
                    setColor(if (on) 0xFFFFFFFF.toInt() else Look.withAlpha(t.fg, 0.35))
                    if (on) setStroke((6 * dp).toInt(), 0x2EFFFFFF)
                }
                if (on) elevation = 8 * dp
            }
            val size = ((if (on) 30 else 8) * dp).toInt()
            val cellView = FrameLayout(host).apply {
                addView(sun, FrameLayout.LayoutParams(size, size, Gravity.CENTER))
                isClickable = true
                isFocusable = true
                setOnClickListener { choose(choice.copy(dir = key, kind = litKind)) }
            }
            zones.addView(cellView, GridLayout.LayoutParams().apply {
                width = 0
                height = 0
                columnSpec = GridLayout.spec(GridLayout.UNDEFINED, 1f)
                rowSpec = GridLayout.spec(GridLayout.UNDEFINED, 1f)
            })
        }
        ui.lightNote.text = if (choice.kind == "flat") host.getString(R.string.theme_light_off) else dirName(choice.dir)

        // Характер света: четыре мини-фона.
        val kinds = ui.kindsRow
        kinds.removeAllViews()
        for ((i, kind) in LookTable.kinds.withIndex()) {
            val on = choice.kind == kind
            val wrap = LinearLayout(host).apply {
                orientation = LinearLayout.VERTICAL
                val pad = (5 * dp).toInt()
                setPadding(pad, pad, pad, (7 * dp).toInt())
                background = GradientDrawable().apply {
                    cornerRadius = minOf(t.r, 12) * dp
                    setColor(if (on) t.accSoft else t.surf)
                    setStroke((1 * dp).toInt(), if (on) t.acc else t.line)
                }
                isClickable = true
                isFocusable = true
                setOnClickListener { choose(choice.copy(kind = kind)) }
            }
            val canvas = View(host).apply {
                background = Backdrop(Look.theme(choice.copy(kind = kind, dir = if (choice.dir == "c" && kind != "radial") "nw" else choice.dir)), Store.PATTERN_NONE)
                outlineProvider = rounded(maxOf(t.r - 6, 4) * dp)
                clipToOutline = true
            }
            wrap.addView(canvas, LinearLayout.LayoutParams(MATCH, (42 * dp).toInt()))
            wrap.addView(
                TextView(host).apply {
                    text = kindName(kind)
                    textSize = 12f
                    gravity = Gravity.CENTER
                    setTextColor(if (on) t.fg else t.dim)
                    typeface = android.graphics.Typeface.create(typeface, android.graphics.Typeface.BOLD)
                    setPadding(0, (6 * dp).toInt(), 0, 0)
                },
                LinearLayout.LayoutParams(MATCH, WRAP),
            )
            kinds.addView(wrap, LinearLayout.LayoutParams(0, WRAP, 1f).apply { if (i > 0) marginStart = (8 * dp).toInt() })
        }

        // Густота тени: ползунок 8…98 — SeekBar считает от нуля, отсюда сдвиг.
        ui.depthSeek.progress = ((choice.depth * 100).toInt() - 8).coerceIn(0, 90)
        tintSeek(ui.depthSeek, t)
        paintDepthNote(choice.depth)
    }

    private fun paintDepthNote(d: Double) {
        ui.depthNote.text = host.getString(
            when {
                d <= 0.3 -> R.string.depth_0
                d <= 0.5 -> R.string.depth_1
                d <= 0.7 -> R.string.depth_2
                d <= 0.85 -> R.string.depth_3
                else -> R.string.depth_4
            },
        )
    }

    private fun tintSeek(seek: SeekBar, t: Theme) {
        seek.progressTintList = android.content.res.ColorStateList.valueOf(t.acc)
        seek.thumbTintList = android.content.res.ColorStateList.valueOf(t.acc)
        seek.progressBackgroundTintList = android.content.res.ColorStateList.valueOf(t.line)
    }

    // ------------------------------------------------------------------ фон

    /** paintPatterns — пять плиток узора: образец фона с узором покрупнее и имя. */
    private fun paintPatterns(t: Theme) {
        val row = ui.patternRow
        row.removeAllViews()
        for ((i, key) in Store.PATTERNS.withIndex()) {
            val on = store.pattern == key
            val wrap = LinearLayout(host).apply {
                orientation = LinearLayout.VERTICAL
                val pad = (4 * dp).toInt()
                setPadding(pad, pad, pad, (6 * dp).toInt())
                background = GradientDrawable().apply {
                    cornerRadius = minOf(t.r, 12) * dp
                    setColor(if (on) t.accSoft else t.surf)
                    setStroke((1 * dp).toInt(), if (on) t.acc else t.line)
                }
                isClickable = true
                isFocusable = true
                setOnClickListener {
                    store.pattern = key
                    onChanged()
                }
            }
            val sample = View(host).apply {
                background = PatternSample(t, key, dp)
                outlineProvider = rounded(maxOf(t.r - 6, 4) * dp)
                clipToOutline = true
            }
            wrap.addView(sample, LinearLayout.LayoutParams(MATCH, (34 * dp).toInt()))
            wrap.addView(TextView(host).apply {
                text = named("pattern_", key)
                textSize = 11f
                gravity = Gravity.CENTER
                maxLines = 1
                setTextColor(if (on) t.fg else t.dim)
                setPadding(0, (5 * dp).toInt(), 0, 0)
            }, LinearLayout.LayoutParams(MATCH, WRAP))
            row.addView(wrap, LinearLayout.LayoutParams(0, WRAP, 1f).apply { if (i > 0) marginStart = (6 * dp).toInt() })
        }
    }

    /** PatternSample — плитка узора поярче, чем на фоне: на образце он должен читаться. */
    private class PatternSample(t: Theme, key: String, dp: Float) : android.graphics.drawable.LayerDrawable(
        arrayOf(
            android.graphics.drawable.ColorDrawable(t.surf2),
            Backdrop(t.copy(kind = "flat", bg = 0), key, ink = 70),
        ),
    )

    /**
     * paintBackdrop рисует «Свой фон»: без фото — одна кнопка выбора, с фото —
     * «другое», «убрать» и ручки. Ручки без фото не показываем: они ничего бы
     * не меняли, а нарисованная ручка — обещание. Видео из макета нет: свой
     * плеер под фон — это ExoPlayer и почти удвоение пакета.
     */
    private fun paintBackdrop(t: Theme) {
        val has = store.hasBackdrop()
        ui.bgNote.text = host.getString(if (has) R.string.theme_backdrop_has else R.string.theme_backdrop_none)
        ui.bgPick.text = host.getString(if (has) R.string.theme_backdrop_change else R.string.theme_backdrop_pick)
        ui.bgPick.setTextColor(if (has) t.dim else t.accFg)
        ui.bgPick.background = Paint.rounded(if (has) t.surf2 else t.acc, minOf(t.r, 12), dp)
        ui.bgDrop.isVisible = has
        ui.bgDrop.setTextColor(t.fail)
        ui.bgDrop.background = Paint.rounded(t.surf2, minOf(t.r, 12), dp)
        ui.bgFrame.isVisible = has
        ui.bgFrame.setTextColor(t.fg)
        ui.bgFrame.background = Paint.rounded(t.surf2, minOf(t.r, 12), dp)
        ui.bgKnobs.isVisible = has
        if (!has) return

        paintPills(ui.bgFits, t, listOf("cover", "contain"), store.backdropFit, ::fitName) {
            store.backdropFit = it
            onChanged()
        }
        ui.bgDimSeek.progress = store.backdropDim
        tintSeek(ui.bgDimSeek, t)
        ui.bgDimNote.text = "${store.backdropDim}%"
    }

    /**
     * photoBackdrop — своё фото под сценой предпросмотра, тем же кадром и той
     * же пеленой, что под приложением. Без него предпросмотр показывал фон
     * темы, а приложение вокруг — фото, и выбирать тему «под фото» было не
     * по чему. Снимок декодируется раз на файл.
     */
    private var photo: android.graphics.Bitmap? = null
    private var photoStamp = 0L

    private fun photoBackdrop(t: Theme, dim: Int = store.backdropDim): android.graphics.drawable.Drawable? {
        val stamp = store.backdropStamp()
        if (stamp == 0L) {
            photo = null
            photoStamp = 0L
            return null
        }
        if (photo == null || photoStamp != stamp) {
            photo = store.backdropBitmap()
            photoStamp = stamp
        }
        val bmp = photo ?: return null
        val a = dim.coerceIn(0, 95) * 255 / 100
        return BackdropImage(bmp, store.backdropFit, t.bg, (a shl 24) or (t.bg and 0x00FFFFFF), store.backdropFrame)
    }

    /** editFrame открывает выбор кадра для своего фона. */
    fun editFrame() {
        val bmp = store.backdropBitmap() ?: return
        val t = Look.theme(store.look)
        val a = store.backdropDim.coerceIn(0, 95) * 255 / 100
        val veil = (a shl 24) or (t.bg and 0x00FFFFFF)
        BackdropEditor(host, bmp, store.backdropFit, t, veil, store.backdropFrame) {
            store.backdropFrame = it
            onChanged()
        }.show()
    }

    private fun fitName(key: String): String =
        host.getString(if (key == "contain") R.string.backdrop_whole else R.string.backdrop_fill)

    private fun rounded(radius: Float) = object : ViewOutlineProvider() {
        override fun getOutline(view: View, outline: Outline) {
            outline.setRoundRect(0, 0, view.width, view.height, radius)
        }
    }

    // ------------------------------------------------------------- таблетки

    private fun paintPills(row: LinearLayout, t: Theme, keys: List<String>, current: String, name: (String) -> String, onPick: (String) -> Unit) {
        row.removeAllViews()
        for ((i, k) in keys.withIndex()) {
            val on = k == current
            row.addView(chip(t, name(k), on) { onPick(k) }, LinearLayout.LayoutParams(0, WRAP, 1f).apply { if (i > 0) marginStart = (8 * dp).toInt() })
        }
    }

    /** chip — кнопка выбора из макета: тихая карточка, выбранная — мягким акцентом с обводкой. */
    private fun chip(t: Theme, label: String, on: Boolean, onPick: () -> Unit): TextView = TextView(host).apply {
        text = label
        textSize = 13f
        gravity = Gravity.CENTER
        maxLines = 1
        minHeight = (40 * dp).toInt()
        setPadding((10 * dp).toInt(), (6 * dp).toInt(), (10 * dp).toInt(), (6 * dp).toInt())
        setTextColor(if (on) t.fg else t.dim)
        typeface = android.graphics.Typeface.create(typeface, android.graphics.Typeface.BOLD)
        background = GradientDrawable().apply {
            cornerRadius = minOf(t.r, 12) * dp
            setColor(if (on) t.accSoft else t.surf)
            setStroke((1 * dp).toInt(), if (on) t.acc else t.line)
        }
        isClickable = true
        isFocusable = true
        setOnClickListener { onPick() }
    }

    // ------------------------------------------------------------------ ещё

    private fun paintLogos(t: Theme) {
        val row = ui.logoRow
        row.removeAllViews()
        for ((i, key) in Store.LOGOS.withIndex()) {
            val on = store.logo == key
            val wrap = LinearLayout(host).apply {
                orientation = LinearLayout.VERTICAL
                gravity = Gravity.CENTER_HORIZONTAL
                setPadding(0, (10 * dp).toInt(), 0, (8 * dp).toInt())
                background = GradientDrawable().apply {
                    cornerRadius = minOf(t.r, 12) * dp
                    setColor(if (on) t.accSoft else t.surf)
                    setStroke((1 * dp).toInt(), if (on) t.acc else t.line)
                }
                isClickable = true
                isFocusable = true
                setOnClickListener {
                    when (key) {
                        // Свой — сначала выбрать картинку; без неё выбирать нечего.
                        Store.LOGO_CUSTOM -> if (store.hasLogo() && !on) { store.logo = key; onChanged() } else onPickLogo()
                        else -> { store.logo = key; onChanged() }
                    }
                }
            }
            val icon: View = when (key) {
                Store.LOGO_MARVIA -> MarviaLogoView(host).apply { setTheme(t) }
                Store.LOGO_CUSTOM -> ImageView(host).apply {
                    val bmp = store.logoBitmap()
                    if (bmp != null) { setImageBitmap(bmp); scaleType = ImageView.ScaleType.CENTER_CROP; clipToOutline = true; outlineProvider = rounded(8 * dp) }
                    else { setImageResource(R.drawable.ic_image); setColorFilter(t.dim) }
                }
                else -> ImageView(host).apply { setImageResource(R.drawable.ic_eye_off); setColorFilter(t.dim) }
            }
            wrap.addView(icon, LinearLayout.LayoutParams((30 * dp).toInt(), (26 * dp).toInt()))
            wrap.addView(TextView(host).apply {
                text = named("logo_", key)
                textSize = 12f
                gravity = Gravity.CENTER
                setTextColor(if (on) t.fg else t.dim)
                typeface = android.graphics.Typeface.create(typeface, android.graphics.Typeface.BOLD)
                setPadding(0, (6 * dp).toInt(), 0, 0)
            })
            row.addView(wrap, LinearLayout.LayoutParams(0, WRAP, 1f).apply { if (i > 0) marginStart = (8 * dp).toInt() })
        }
        ui.logoNote.text = host.getString(
            when (store.logo) {
                Store.LOGO_CUSTOM -> R.string.theme_logo_custom
                Store.LOGO_NONE -> R.string.theme_logo_none
                else -> R.string.theme_logo_marvia
            },
        )
    }

    private fun paintFonts(t: Theme) {
        val row = ui.fontRow
        row.removeAllViews()
        for ((i, key) in Store.FONTS.withIndex()) {
            val on = store.font == key
            val c = chip(t, named("font_", key), on) { store.font = key; onChanged() }
            val family = ResourcesCompat.getFont(host, when (key) { "manrope" -> R.font.manrope; "geologica" -> R.font.geologica; else -> R.font.onest })
            c.typeface = android.graphics.Typeface.create(family, android.graphics.Typeface.BOLD)
            c.setTag(R.id.keep_font, true)
            row.addView(c, LinearLayout.LayoutParams(0, WRAP, 1f).apply { if (i > 0) marginStart = (8 * dp).toInt() })
        }
    }

    private fun paintProfiles(t: Theme) {
        val codes = store.profiles
        val row = ui.profileRow
        row.removeAllViews()
        for (i in 0 until 3) {
            val on = slot == i
            val code = codes[i]
            val c = chip(t, host.getString(if (code != null) R.string.profile_full else R.string.profile_empty, i + 1), on) {
                slot = i
                val next = code?.let { Look.decode(it) }
                if (next != null) choose(next, store.profileStyle(i)) else paint(t)
            }
            row.addView(c, LinearLayout.LayoutParams(0, WRAP, 1f).apply { if (i > 0) marginStart = (8 * dp).toInt() })
        }
        ui.profileSave.background = Paint.rounded(t.acc, minOf(t.r, 12), dp)
        ui.profileSave.setTextColor(t.accFg)
        ui.themeRandom.background = Paint.rounded(t.surf2, minOf(t.r, 12), dp)
        ui.themeRandom.setTextColor(t.fg)
    }

    private fun View.doOnLayoutOnce(block: () -> Unit) {
        addOnLayoutChangeListener(object : View.OnLayoutChangeListener {
            override fun onLayoutChange(v: View, l: Int, tp: Int, r: Int, b: Int, ol: Int, ot: Int, or: Int, ob: Int) {
                block()
            }
        })
    }

    private companion object {
        const val MATCH = LinearLayout.LayoutParams.MATCH_PARENT
        const val WRAP = LinearLayout.LayoutParams.WRAP_CONTENT
        val LAMP = listOf("nw", "n", "ne", "w", "c", "e", "sw", "s", "se")
        /** Мегабайты по часам для предпросмотра — как в макете. */
        val MINI = listOf(2, 1, 0, 0, 0, 0, 3, 6, 12, 16, 11, 9, 13, 18, 17, 10, 9, 15, 22, 22, 20, 12, 5, 0)
        // Предпросмотр скорости: неровные замеры в байтах в секунду, как у живой загрузки.
        val SPARK_DOWN = listOf(2.1, 2.4, 3.8, 3.1, 4.6, 5.9, 5.2, 6.4, 4.8, 5.5, 7.2, 6.1, 6.8, 5.7, 6.0).map { it * 1_000_000 }
        val SPARK_UP = listOf(0.3, 0.4, 0.6, 0.5, 0.9, 0.7, 0.8, 1.1, 0.6, 0.7, 0.9, 0.8, 1.0, 0.7, 0.78).map { it * 1_000_000 }
    }
}
