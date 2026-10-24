<div align="center">

<img src="docs/shots/readme-brand.png" alt="Marvia" width="1000">

<div dir="rtl">

**وی‌پی‌ان خودتان، از سر تا ته: پروتکل، نودها، پنل و برنامه برای اندروید و ویندوز.**

کسی که دسترسی می‌دهد، پنل را با یک اسکریپت نصب می‌کند و هر نود را با یک خط
که از پنل کپی می‌کند اضافه می‌کند. کسی که وصل می‌شود فقط یک لینک می‌گیرد:
برنامه خودش نود را انتخاب می‌کند و اگر نود جواب ندهد، سراغ نود سالم می‌رود.

</div>

[Русский](README.md) · [English](README.en.md) · [简体中文](README.zh-CN.md) · [فارسی](README.fa.md)

<a href="https://github.com/jytt8u/marvia/releases/tag/v0.13.0-alpha.28"><img alt="دانلود آخرین نسخه" src="https://img.shields.io/github/v/release/jytt8u/marvia?include_prereleases&display_name=tag&style=for-the-badge&label=%D8%AF%D8%A7%D9%86%D9%84%D9%88%D8%AF%20%D8%A2%D8%AE%D8%B1%DB%8C%D9%86%20%D9%86%D8%B3%D8%AE%D9%87&labelColor=30353b&color=c3ccd6" height="40"></a>

<sub>APK اندروید · نصب‌کنندهٔ ویندوز · بسته‌های سرور · چک‌سام‌ها</sub>

[![Android APK](https://img.shields.io/badge/ANDROID-APK-687482?style=for-the-badge&labelColor=30353b)](https://github.com/jytt8u/marvia/releases/download/v0.13.0-alpha.28/marvia-android.apk)
[![Windows test](https://img.shields.io/badge/WINDOWS-TEST-687482?style=for-the-badge&labelColor=30353b)](https://github.com/jytt8u/marvia/releases/tag/v0.13.0-alpha.28)
[![Panel](https://img.shields.io/badge/PANEL-INSTALL-687482?style=for-the-badge&labelColor=30353b)](#نصب-پنل)
[![Docs](https://img.shields.io/badge/DOCS-GUIDE-687482?style=for-the-badge&labelColor=30353b)](docs/guide.md)

</div>

<div dir="rtl">

**مرحله: آلفا، `0.13.0-alpha.28`.** نسخهٔ 1.0 تا آزمایش روی دستگاه‌ها و
شبکه‌های واقعی عقب افتاده است — [معیارها (روسی)](docs/development-plan.md).
تازه‌ترین نسخهٔ آزمایشی [v0.13.0-alpha.28](https://github.com/jytt8u/marvia/releases/tag/v0.13.0-alpha.28)
است و پایدار اعلام نشده است.

بیشتر مستندات فعلاً به روسی است؛ نسخهٔ [انگلیسی](README.en.md) کامل‌تر است.

## برنامه

اندروید · تصاویر واقعی شبیه‌ساز نسخهٔ 0.12.2 · بدون کلید

</div>

| صفحهٔ اصلی | تنظیمات VPN | شبکه و حریم خصوصی |
|:---:|:---:|:---:|
| <img src="docs/shots/android-home.png" alt="Home" width="260"> | <img src="docs/shots/android-settings.png" alt="VPN settings" width="260"> | <img src="docs/shots/android-advanced.png" alt="Network and privacy" width="260"> |

<div dir="rtl">

## معماری

</div>

<img src="docs/shots/readme-flow.svg" alt="Access control is separate from the VPN data path" width="1200">

<div dir="rtl">

پنل دسترسی را مدیریت می‌کند؛ بسته‌های VPN از نود می‌گذرند، نه از پنل.
[مرزهای اعتماد](docs/architecture.md).

## امنیت VP1

</div>

<img src="docs/shots/readme-security-en.svg" alt="Noise inside TLS, compared with WireGuard, VLESS and Trojan" width="1200">

<div dir="rtl">

**یک نشست Noise جداگانه درون TLS:** بررسی کلید نود و رمزگذاری داده‌ها فقط به
کانال TLS بیرونی وابسته نیست. این یک تفاوت معماری است، نه اثبات این‌که VP1 از
همهٔ گزینه‌های دیگر امن‌تر است.
[مقایسه، منابع و مرزهای حفاظت](docs/security-comparison.md).

در فهرست دسترسی VP1 **هیچ کلید خصوصی کاربری** نیست؛ نود فقط کلیدهای عمومی را
لازم دارد. [بررسی خودکار ۲۶ سپتامبر ۲۰۲۶](docs/security-checks/2026-09-26/README.md):
پس از به‌روزرسانی وابستگی‌ها هیچ آسیب‌پذیری شناخته‌شدهٔ قابل‌دسترسی پیدا نشد؛
۸٫۵۵ میلیون اجرای چهار آزمون fuzz بدون خطا. این ممیزی مستقل نیست.

**فروشنده چه می‌بیند و چه نمی‌بیند.** پنل نشانی IP خریداران را نگه نمی‌دارد؛
نود آن را فقط برای محدودیت تعداد دستگاه، در حافظه و به مدت یک ساعت نگه می‌دارد؛
لاگ‌ها نه نشانی می‌نویسند نه سایت. هر نود خروجی چه چیزی را همچنان می‌بیند و VPN
از چه چیزی محافظت نمی‌کند — [docs/privacy.md](docs/privacy.md).

## سرعت VP1

در آزمون محلی، VP1 جدید **۵۶٪ بیشتر از نسخهٔ قبلی خودش** داده جابه‌جا می‌کند
(۴۳۵٫۰ ← ۶۷۸٫۳ مگابایت بر ثانیه، پنج اجرای جفتی ۵۱۲ مگابایتی روی یک رایانه، بدون
اینترنت و TUN). این افزایش توان محلی است، نه وعدهٔ اینترنت ۵۶٪ سریع‌تر. در
مقایسهٔ کامل محلی، VLESS و Trojan هنوز از VP1 سریع‌ترند.
[داده‌ها](docs/benchmarks/2026-09-26-record-fit/README.md) ·
[مقایسه با VLESS و Trojan](docs/performance.md)

## پروتکل‌ها

WireGuard و OpenVPN بسته‌های IP را تونل می‌کنند؛ بقیه پراکسی‌اند. در اندروید،
Marvia برای کلیدهای پراکسی دیگران هم رابط VPN سیستمی می‌سازد.

</div>

| پروتکل | انتقال و تفاوت | پشتیبانی در Marvia |
|---|---|---|
| **[VP1](docs/protocol.md)** | پراکسی Noise روی TLS/REALITY، WebSocket یا QUIC؛ اگر UDP در دسترس نباشد به TCP برمی‌گردد | نود خودی، اندروید و ویندوز |
| [WireGuard](https://www.wireguard.com/protocol/) | تونل IP روی UDP؛ پوشش HTTPS ندارد | اندروید: کلید دیگران |
| [OpenVPN](https://openvpn.net/community-docs/community-articles/openvpn-2-7-manual.html) | تونل IP روی UDP یا TCP با TLS؛ پروتکلی جدا، نه HTTPS معمولی | پشتیبانی نمی‌شود |
| [VLESS + REALITY](https://xtls.github.io/en/config/transports/reality.html) | پراکسی TCP با دست‌دادن TLS شبیه سایت هدف | نود و اندروید |
| [Trojan](https://github.com/trojan-gfw/trojan/blob/master/docs/protocol.md) | پراکسی درون TLS با سایت پوششی | نود و اندروید |
| [Shadowsocks](https://shadowsocks.org/doc/what-is-shadowsocks.html) | پراکسی رمزشدهٔ TCP/UDP؛ به‌تنهایی شبیه HTTPS نیست | اندروید: کلید دیگران |
| [Hysteria 2](https://v2.hysteria.network/docs/developers/Protocol/) | پراکسی QUIC/UDP با ظاهر HTTP/3؛ UDP باید کار کند | اندروید: کلید دیگران |

<div dir="rtl">

VP1 با کلاینت‌های WireGuard یا Xray سازگار نیست.

## چه چیزهایی دارد

- **اندروید:** کلیدهای Marvia و دیگران، صفحهٔ اصلی به انتخاب کاربر، از یک دکمهٔ ساده تا کارت کشور و اشتراک، خواندن QR از گالری و دوربین بدون
  سرویس‌های گوگل، انتخاب نود، آمار، تنظیمات VPN، پرس‌وجوی نام‌ها از راه HTTPS
  پنهان از نود، دکمه‌های «تمدید» و «پشتیبانی» فروشنده، به‌روزرسانی اشتراک و
  یادآوری در پس‌زمینه.
- **ویندوز:** کلاینت TUN، انتخاب DNS و رمزگذاری پرس‌وجوی نام‌ها، تکه‌کردن
  پیام آغازین TLS، چند کلید، مصرف هفتگی، کلید خاموش‌کردن IPv6، «تمدید» و
  «پشتیبانی» فروشنده. گزینهٔ «سایت‌های روسی مستقیم» فقط فهرست زیرشبکه‌های
  روسیه را از تونل بیرون می‌برد؛ فهرستی برای سایت‌های ایرانی وجود ندارد.
- **پنل و نودها:** محدودیت‌ها، حساب مصرف، تعرفه‌ها و تمدید با دو کلیک، در صفحهٔ اصلی: چه کسی به‌زودی تمام می‌کند و مصرف هر نود، QR و متن
  آماده برای تلگرام، پشتیبان‌گیری روزانهٔ رمزشده، API ربات و وب‌هوک، انتقال از
  Marzban و 3x-ui؛ صفحهٔ خریدار با لینک اشتراک؛ اشتراک برای Happ، v2RayTun و
  Hiddify و پروفایل آماده برای sing-box و Clash.
- **سرور:** VP1، VLESS و Trojan؛ به‌روزرسانی نودها با بررسی امضا.

## مقایسه با پروژه‌های دیگر

</div>

| پروژه | تمرکز | نقطهٔ قوت |
|---|---|---|
| **Marvia** | پنل + نودها + اندروید/ویندوز خودی + VP1 | یک بسته برای خریدار و فروشنده |
| [Marzban](https://github.com/Gozargah/Marzban) | پنل Xray | سهمیهٔ دوره‌ای و تلگرام |
| [Remnawave](https://docs.rw/) | پنل و نودهای Xray | قالب‌های Mihomo/sing-box، کنترل دستگاه‌ها |
| [3x-ui](https://docs.sanaei.dev/docs/) | پنل Xray | پروتکل‌ها و ابزارهای مدیریتی فراوان |

<div dir="rtl">

Marvia هنوز ریست خودکار ماهانهٔ سهمیه ندارد. خریداران از Marzban و 3x-ui با همان
کلیدها و نشانی‌های اشتراک منتقل می‌شوند —
[چه چیزی در این انتقال عوض می‌شود (روسی)](docs/guide.md#переезд-с-marzban-и-3x-ui).
مقایسه بر پایهٔ مستندات همان پروژه‌هاست؛ مقایسهٔ سرعت هم‌سنجی‌شده‌ای وجود ندارد.

## شروع

> **کلید دارید؟** [APK اندروید](https://github.com/jytt8u/marvia/releases/download/v0.13.0-alpha.28/marvia-android.apk)
> یا [نصب‌کنندهٔ ویندوز](https://github.com/jytt8u/marvia/releases/tag/v0.13.0-alpha.28)
> (`marvia-windows-setup.exe`) را بگیرید، لینک `marvia://…` را در برنامه اضافه
> کنید و وصل شوید.

اندروید لینک‌های VLESS، VMess، Trojan، Shadowsocks، Hysteria2 و WireGuard را هم
می‌پذیرد. برنامه هنوز در Google Play نیست.

نسخهٔ عمومی alpha.28 آزمایشی است.
آزمایش روی دستگاه‌های واقعی هنوز تمام نشده است.

### بررسی اصالت فایل‌ها

**APK** از نخستین نسخه با یک کلید امضا شده است. اثر انگشت SHA-256 گواهی:

</div>

```
53:EB:5B:D4:4A:38:1C:BA:E1:9B:76:06:EF:20:91:9E:33:DF:FB:26:0D:49:F2:D4:AD:86:88:85:36:4D:7E:82
```

<div dir="rtl">

روی گوشی با [AppVerifier](https://github.com/soupslurpr/AppVerifier) و روی رایانه
با `apksigner verify --print-certs marvia-android.apk` از Android SDK بررسی کنید.
اثر انگشت دیگر یعنی فایل ما نیست. پس از نصب، اندروید به‌روزرسانی با امضای دیگر را
نمی‌پذیرد.

**APK از 0.13.0-alpha.3 به بعد، پنل، نود و ویندوز** روی GitHub از برچسب ساخته
می‌شوند. APK نسخه‌های قدیمی‌تر را نویسنده روی رایانهٔ خودش ساخته است: امضای آن‌ها
نشان می‌دهد چه کسی منتشر کرده، نه این‌که از کدام کد ساخته شده‌اند. جمع‌های
کنترلی همهٔ فایل‌ها با Sigstore امضا شده‌اند:

</div>

```bash
gh attestation verify SHA256SUMS --bundle SHA256SUMS.sigstore.json --repo jytt8u/marvia && sha256sum -c SHA256SUMS --ignore-missing
```

<div dir="rtl">

### نصب پنل

یک سرور لینوکس و دامنه‌ای با رکورد A لازم است. نصب‌کننده از
[نسخهٔ آزمایشی v0.12.2](https://github.com/jytt8u/marvia/releases/tag/v0.12.2)
گرفته می‌شود. روی یک VPS، پنل از درگاه 8443 و نود از 443 استفاده می‌کند.
آماده‌سازی کامل و بررسی جمع‌ها در [راهنمای از صفر (روسی)](docs/start-from-zero.md):

</div>

```bash
curl -fsSL https://github.com/jytt8u/marvia/releases/download/v0.12.2/install-panel.sh -o install-panel.sh
sudo sh install-panel.sh --domain panel.example.com --email you@example.com --port 8443 --from https://github.com/jytt8u/marvia/releases/download/v0.12.2/marvia_linux_amd64.tar.gz
```

<div dir="rtl">

توکن مدیر را که هنگام نصب نشان داده می‌شود نگه دارید، سپس در پنل نود و مشتری
بسازید. [راهنمای کامل (روسی)](docs/guide.md) · [API ربات](docs/bot.md).

## آنچه هنوز آماده نیست

- Google Play: آزمایش نسخهٔ جدید روی گوشی، اظهارنامه‌ها و آزمون.
- ریست خودکار ماهانهٔ سهمیه. در انتقال از Marzban و 3x-ui: VMess، Shadowsocks،
  لینک‌های Vision و پایگاه‌داده‌های MySQL و PostgreSQL منتقل نمی‌شوند.
- iOS، پذیرش اشتراک Clash/sing-box در برنامه‌های خودمان، TUIC، افزونه‌های
  Shadowsocks و لغو جداگانهٔ دستگاه‌ها با کلید مشترک.
- در ویندوز: برنامه‌های بیرون از تونل، kill switch و خواندن کلید از
  تصویر QR — این‌ها فعلاً فقط در گوشی هست.
- رمزگذاری پرس‌وجوی نام‌ها (DoH) با آزمون‌ها بررسی شده، ولی هنوز روی گوشی و
  رایانهٔ واقعی نه. نام‌ها را از نود پنهان می‌کند، اما نه نشانی سایت‌ها و نه نام
  در پیام آغازین TLS (SNI) را.

Marvia دسترسی VPN نمی‌فروشد، پرداخت نمی‌گیرد و سرور میزبانی نمی‌کند.

## مجوز

[AGPL-3.0](LICENSE). استفاده، تغییر و انتشار آزاد است. اگر پنل، نود یا برنامهٔ
تغییریافته را توزیع کنید — از جمله با ارائهٔ آن از راه شبکه — تغییرات خود را با
همین مجوز منتشر می‌کنید.

</div>
