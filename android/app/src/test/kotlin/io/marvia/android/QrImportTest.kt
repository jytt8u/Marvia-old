package io.marvia.android

import com.google.zxing.BarcodeFormat
import com.google.zxing.EncodeHintType
import com.google.zxing.qrcode.QRCodeWriter
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test
import java.nio.ByteBuffer

class QrImportTest {
    /**
     * QR-код посреди «скриншота»: белое поле с отступом, как в чате. Пиксели
     * рисуем сами, без Bitmap, — так тест идёт без телефона.
     */
    private fun screenshot(text: String, scale: Int = 4, margin: Int = 120): Triple<Int, Int, IntArray> {
        // UTF-8 — так кодируют панели, боты и генераторы QR: имя ноды по-русски.
        val hints = mapOf(EncodeHintType.CHARACTER_SET to "UTF-8")
        val matrix = QRCodeWriter().encode(text, BarcodeFormat.QR_CODE, 0, 0, hints)
        val width = matrix.width * scale + margin * 2
        val height = matrix.height * scale + margin * 2
        val pixels = IntArray(width * height) { 0xFFF2F2F2.toInt() }
        for (y in 0 until matrix.height) for (x in 0 until matrix.width) {
            if (!matrix[x, y]) continue
            for (dy in 0 until scale) for (dx in 0 until scale) {
                pixels[(margin + y * scale + dy) * width + margin + x * scale + dx] = 0xFF101010.toInt()
            }
        }
        return Triple(width, height, pixels)
    }

    @Test
    fun keyFromQrScreenshotIsReadExactly() {
        val key = "marvia://k3j9x2mq8w1z4v7pQwErTy@panel.example.com/sub/AbCdEf0123456789#Мой доступ"
        val (w, h, px) = screenshot(key)
        assertEquals(key, QrImport.decode(w, h, px))
    }

    @Test
    fun foreignVlessLinkIsReadToo() {
        val link = "vless://6f1a6c2e-3d4b-4e5f-8a9b-0c1d2e3f4a5b@198.51.100.7:443?security=reality&pbk=abc&sid=01&sni=www.microsoft.com&type=tcp#Финляндия"
        val (w, h, px) = screenshot(link)
        assertEquals(link, QrImport.decode(w, h, px))
    }

    /** Скриншот QR с экрана компьютера: модуль в восемь точек. Такой ZXing
     *  сам не находил — читается только после уменьшения. */
    @Test
    fun largeCrispQrFromComputerScreenIsRead() {
        val link = "vless://6f1a6c2e-3d4b-4e5f-8a9b-0c1d2e3f4a5b@198.51.100.7:443?security=reality&sni=www.microsoft.com#Финляндия"
        val (w, h, px) = screenshot(link, scale = 8)
        assertEquals(link, QrImport.decode(w, h, px))
    }

    /** Переезд на новый телефон: показанный код читается тем же сканером. */
    @Test
    fun shownKeyIsScannedBackExactly() {
        val key = "marvia://k3j9x2mq8w1z4v7pQwErTy@panel.example.com/sub/AbCdEf0123456789#Мой доступ"
        val side = 600
        assertEquals(key, QrImport.decode(side, side, QrImport.encode(key, side)))
    }

    @Test
    fun pictureWithoutQrGivesNothing() {
        val px = IntArray(400 * 300) { if (it % 7 == 0) 0xFF000000.toInt() else 0xFFFFFFFF.toInt() }
        assertNull(QrImport.decode(400, 300, px))
    }

    @Test
    fun cameraYuvFrameReadsTheSameKeyWithPaddingCropAndRotation() {
        val key = "marvia://k3j9x2mq8w1z4v7pQwErTy@panel.example.com/sub/AbCdEf0123456789#Мой доступ"
        val (width, height, pixels) = screenshot(key)
        val frameHeight = height + 40
        for (pixelStride in listOf(1, 2)) for (rotation in listOf(0, 90, 180, 270)) {
            val left = 8
            val top = 5
            val offset = 11
            val rowStride = (width + left) * pixelStride + 17
            // Ненулевая позиция, межстрочные отступы и U/V после Y:
            // декодер должен увидеть только яркость выбранного кадра.
            val ySize = (frameHeight + top) * rowStride
            val yuv = ByteBuffer.allocateDirect(offset + ySize + ySize / 2)
            for (i in 0 until yuv.limit()) yuv.put(i, 0x80.toByte())
            for (y in 0 until frameHeight) for (x in 0 until width) {
                val luma = if (y < height) pixels[y * width + x] and 0xff else 0xf2
                yuv.put(offset + (y + top) * rowStride + (x + left) * pixelStride, luma.toByte())
            }
            yuv.position(offset)
            assertEquals("шаг $pixelStride, поворот $rotation", key,
                QrImport.decodeYuv(width, frameHeight, yuv.asReadOnlyBuffer(), rowStride, pixelStride, rotation, left, top))
            assertEquals(offset, yuv.position())
        }
    }

    @Test
    fun blankOrTruncatedCameraFrameDoesNotProduceAKey() {
        assertNull(QrImport.decodeYuv(320, 240, ByteBuffer.wrap(ByteArray(320 * 240) { 120 }), 320, 1))
        assertNull(QrImport.decodeYuv(320, 240, ByteBuffer.allocate(10), 320, 1))
        assertNull(QrImport.decodeYuv(0, 240, ByteBuffer.allocate(10), 320, 1))
    }
}
