package io.marvia.android

import android.content.Context
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.net.Uri
import com.google.zxing.BarcodeFormat
import com.google.zxing.BinaryBitmap
import com.google.zxing.DecodeHintType
import com.google.zxing.EncodeHintType
import com.google.zxing.NotFoundException
import com.google.zxing.RGBLuminanceSource
import com.google.zxing.LuminanceSource
import com.google.zxing.PlanarYUVLuminanceSource
import com.google.zxing.ReaderException
import com.google.zxing.common.GlobalHistogramBinarizer
import com.google.zxing.common.HybridBinarizer
import com.google.zxing.qrcode.QRCodeReader
import com.google.zxing.qrcode.QRCodeWriter
import com.google.zxing.qrcode.decoder.ErrorCorrectionLevel
import java.nio.ByteBuffer

/**
 * QrImport — один декодер ключа для картинки и кадра камеры.
 *
 * Продавцы часто присылают ключ не строкой, а QR-кодом — скриншотом в чат, —
 * и покупатель переписывал бы его руками, если бы было с чего. Распознаём с
 * картинки из галереи: для этого не нужно разрешение на камеру, а скриншот —
 * самый частый случай.
 *
 * ZXing core, а не ML Kit: тот требует сервисов Google, а приложение ставят и
 * на телефоны без них, и раздают мимо магазина.
 */
object QrImport {
    /**
     * Самая длинная сторона, до которой ужимаем картинку. Скриншот в 4000
     * точек распознаётся не лучше, чем в 1600, а памяти и времени требует
     * вшестеро больше.
     */
    private const val MAX_SIDE = 1600

    /** Ключ с картинки; null — QR-кода на ней нет или он не прочитался. */
    fun fromUri(context: Context, uri: Uri): String? {
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        context.contentResolver.openInputStream(uri)?.use { BitmapFactory.decodeStream(it, null, bounds) }
        if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return null

        var sample = 1
        while (maxOf(bounds.outWidth, bounds.outHeight) / (sample * 2) >= MAX_SIDE) sample *= 2
        val opts = BitmapFactory.Options().apply { inSampleSize = sample }
        val bitmap = context.contentResolver.openInputStream(uri)?.use {
            BitmapFactory.decodeStream(it, null, opts)
        } ?: return null
        return try {
            fromBitmap(bitmap)
        } finally {
            bitmap.recycle()
        }
    }

    private fun fromBitmap(bitmap: Bitmap): String? {
        val pixels = IntArray(bitmap.width * bitmap.height)
        bitmap.getPixels(pixels, 0, bitmap.width, 0, 0, bitmap.width, bitmap.height)
        return decode(bitmap.width, bitmap.height, pixels)
    }

    /**
     * encode рисует ключ QR-кодом — для переезда на новый телефон: старый
     * показывает, новый сканирует камерой «Добавить ключ». Пиксели ARGB
     * квадратом side×side, с белым полем вокруг: без поля сканеры теряют
     * искатели на тёмной теме. Уровень коррекции M — ключ длинный, и код
     * с H на экране телефона вышел бы слишком мелким.
     */
    fun encode(text: String, side: Int): IntArray {
        // UTF-8: в ключе бывает имя по-русски, а по умолчанию ZXing пишет Latin-1.
        val hints = mapOf(
            EncodeHintType.CHARACTER_SET to "UTF-8",
            EncodeHintType.ERROR_CORRECTION to ErrorCorrectionLevel.M,
            EncodeHintType.MARGIN to 4,
        )
        val matrix = QRCodeWriter().encode(text, BarcodeFormat.QR_CODE, side, side, hints)
        return IntArray(side * side) { i -> if (matrix.get(i % side, i / side)) BLACK else WHITE }
    }

    private const val BLACK = 0xFF000000.toInt()
    private const val WHITE = 0xFFFFFFFF.toInt()

    /**
     * Распознаёт QR-код в пикселях ARGB. Отдельно от Bitmap, чтобы проверять
     * его обычным тестом, без телефона.
     */
    fun decode(width: Int, height: Int, pixels: IntArray): String? {
        return decodeSource(RGBLuminanceSource(width, height, pixels))
    }

    /**
     * QR различается по яркости, поэтому U и V не нужны. У разных камер
     * строки Y дополнены до разной длины: копирование всего буфера подряд
     * исказило бы изображение. Позиция буфера и отступ кадра тоже учитываются.
     */
    fun decodeYuv(
        width: Int, height: Int, yPlane: ByteBuffer, rowStride: Int, pixelStride: Int,
        rotationDegrees: Int = 0, left: Int = 0, top: Int = 0,
    ): String? {
        if (width <= 0 || height <= 0 || rowStride <= 0 || pixelStride <= 0 || left < 0 || top < 0) return null
        if (rotationDegrees !in listOf(0, 90, 180, 270)) return null
        val start = yPlane.position().toLong() + top.toLong() * rowStride + left.toLong() * pixelStride
        val end = start + (height - 1L) * rowStride + (width - 1L) * pixelStride
        if (end >= yPlane.limit() || width.toLong() * height > Int.MAX_VALUE) return null
        val rotated = rotationDegrees == 90 || rotationDegrees == 270
        val w = if (rotated) height else width
        val h = if (rotated) width else height
        val packed = ByteArray(w * h)
        for (y in 0 until height) for (x in 0 until width) {
            val index = when (rotationDegrees) {
                90 -> x * w + height - 1 - y
                180 -> (height - 1 - y) * w + width - 1 - x
                270 -> (width - 1 - x) * w + y
                else -> y * w + x
            }
            packed[index] = yPlane.get((start + y.toLong() * rowStride + x.toLong() * pixelStride).toInt())
        }
        return decodeSource(PlanarYUVLuminanceSource(packed, w, h, 0, 0, w, h, false))
    }

    private fun decodeSource(original: LuminanceSource): String? {
        // Сначала как есть, потом мельче. ZXing не находит крупный и очень
        // чёткий код: при модуле больше трёх точек он теряет искатели, хотя
        // тот же код вдвое мельче читает уверенно. Скриншот QR с компьютера —
        // ровно такой случай, поэтому уменьшаем, как это делают сканеры.
        var source = original
        for (step in 0..2) {
            readAny(source)?.let { return it }
            if (source.width < 200 || source.height < 200) break
            source = halve(source)
        }
        // Последняя попытка — код без фона вокруг: вырезанный из окна или
        // присланный файлом картинки.
        return read(BinaryBitmap(HybridBinarizer(original)),
            mapOf(DecodeHintType.PURE_BARCODE to true))
    }

    private fun readAny(source: LuminanceSource): String? {
        val hints = mapOf(
            DecodeHintType.POSSIBLE_FORMATS to listOf(BarcodeFormat.QR_CODE),
            // Скриншот чата: код мелкий, вокруг текст и пузыри сообщений.
            DecodeHintType.TRY_HARDER to true,
        )
        // Два способа отделить чёрное от белого: локальный хорош на фото с
        // неровным светом, общий — на чистом скриншоте.
        return read(BinaryBitmap(HybridBinarizer(source)), hints)
            ?: read(BinaryBitmap(GlobalHistogramBinarizer(source)), hints)
    }

    private fun read(binary: BinaryBitmap, hints: Map<DecodeHintType, Any>): String? {
        val text = try {
            QRCodeReader().decode(binary, hints).text
        } catch (_: NotFoundException) {
            null
        } catch (_: ReaderException) {
            null
        }
        return text?.trim()?.takeIf { it.isNotEmpty() }
    }

    private fun halve(source: LuminanceSource): LuminanceSource {
        val width = source.width
        val height = source.height
        val pixels = source.matrix
        val w = width / 2
        val h = height / 2
        val out = ByteArray(w * h)
        for (y in 0 until h) for (x in 0 until w) {
            var sum = 0
            for (dy in 0..1) for (dx in 0..1) {
                sum += pixels[(y * 2 + dy) * width + x * 2 + dx].toInt() and 0xFF
            }
            out[y * w + x] = (sum / 4).toByte()
        }
        return PlanarYUVLuminanceSource(out, w, h, 0, 0, w, h, false)
    }
}
