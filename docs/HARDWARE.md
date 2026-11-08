# Hardware

## Boards

| Board | Verdict | Why |
|---|---|---|
| **Raspberry Pi 4 B**, **Pi 400** | Good | Gigabit Ethernet is separate from USB; USB ports come from a dedicated controller; dual-band WiFi |
| **Raspberry Pi 5** | Good | As above, with more headroom. Needs the 27 W supply for full USB power |
| **Raspberry Pi 500 / 500+** | Probably good, untried | The Pi 5's SoC in a keyboard. Use Raspberry Pi OS and the installer; the appliance card does **not** support it (the pinned kernel has no device tree for it, so the card carries none) |
| **CM4 / CM5** | Good in principle | Same SoCs. Needs a carrier board with USB, and its USB 2.0 port may need switching to host mode in `config.txt` (see Raspberry Pi's compute module documentation). The appliance image installs no device-tree overlays, so use Raspberry Pi OS for these until that is sorted out. The appliance card's device trees cover the CM4, CM4S and the CM5 (on the CM5 IO and CM4 IO boards); the **CM5 Lite** has none in the pinned kernel, so it needs Raspberry Pi OS |
| Pi 3 B/B+ | Works, compromised | The Ethernet chip hangs off the same USB 2.0 bus as your MIDI devices, so a sample transfer and network traffic compete. 64-bit OS only |
| Pi Zero 2 W | Works, compromised | One USB data port (micro-USB OTG), so you need an OTG hub with power injection; no Ethernet unless you spend a USB port on an adapter; **2.4 GHz WiFi only**, which Bluetooth shares |
| Pi 1, 2, Zero, Zero W | Not supported | 32-bit (armv6/armv7) only |

The released binary and the appliance image are 64-bit.

## USB hub

EMM talks to everything through one hub, so the hub matters more than anything else you can buy.

* **Powered**, with a proper supply. The Pi cannot power several bus-powered devices.
* **USB 2.0 high-speed** is what you want. USB MIDI is USB 2.0: devices that are high-speed run at 480 Mbit/s, full-speed ones at 12 Mbit/s. A USB 1.1 hub would force everything to 12 Mbit/s.
* Prefer a **multi-TT** hub (one transaction translator per port). With a single-TT hub, all full-speed devices behind it share one 12 Mbit/s channel, which hurts when several synths do SysEx dumps at once. High-speed devices are not affected.
* Avoid very cheap hubs: resets, glitches and port-power quirks look like "random" MIDI dropouts.
* USB 3 hubs are fine; MIDI devices still run at USB 2.0 speeds.
* Put big-transfer devices (samplers) on a different hub port from chatty ones where you can.

## Power

* Pi 4: the official 5.1 V / 3 A supply. The four USB ports share one current limit (commonly quoted as 1.2 A in total), so anything hungry needs a powered hub.
* Pi 5: the official 27 W (5.1 V / 5 A) supply. Per Raspberry Pi's [power-supply product brief](https://datasheets.raspberrypi.com/power-supply/27w-usb-c-power-supply-product-brief.pdf) the USB current limit rises from 600 mA to 1.6 A when it detects it. With a weaker supply the limit stays at 600 mA.
* Either way, a powered hub keeps MIDI gear's draw off the Pi.
* An undervoltage warning ("Under-voltage detected" in `dmesg`) is the first thing to rule out when USB devices disconnect.
* Pulling the plug is safe on the appliance image (see [SDCARD-IMAGE.md](SDCARD-IMAGE.md)); on Raspberry Pi OS shut down properly (`sudo poweroff`, or the web interface's power menu).

## WiFi, Bluetooth and timing

* The Pi's WiFi and Bluetooth share one antenna and the 2.4 GHz band. On 2.4 GHz, WiFi traffic (a big RTP-MIDI transfer) can disturb Bluetooth MIDI, and the reverse. **Use 5 GHz WiFi, or Ethernet.** Zero 2 W has no 5 GHz.
* RTP-MIDI over WiFi has no guarantees on latency or jitter. For tight timing (clock, live playing), use Ethernet. For remote control and patch uploads, WiFi is fine.
* EMM turns WiFi power-save off, which removes tens of milliseconds of jitter.
* Bluetooth LE MIDI latency depends on the connection interval the other side chooses; expect it to be noticeably worse than USB or Ethernet.
* Set `wifi_country` (see [INSTALL.md](INSTALL.md)): the regulatory domain decides which 5 GHz channels you may use.

## 5-pin DIN MIDI (later)

Not part of this version. The Pi's UART can be turned into a MIDI port (31250 baud) with a small optocoupler circuit or a HAT, and EMM's router could treat it as another port. It would need a new backend in `internal/device` and a `dtoverlay=` line, and the appliance image installs no overlays today. A USB MIDI interface with DIN connectors is the practical answer until then.

## Case notes

* Keep the Pi cooled when it sits in a box next to synths: a Pi 4/5 without a heatsink throttles when it is warm, and the web interface can show the CPU temperature. A passive case is silent, which matters in a studio. A Pi 5 benefits from the official active cooler.
* Leave the SD card slot reachable if you use the appliance image: updating means swapping files on the card.
* Route the USB cable from the Pi to the hub with a short, good cable; thin long cables are a common cause of dropouts.
* The Pi's LEDs and HDMI output are the only feedback on the appliance image besides the web interface; HDMI shows the boot messages on a monitor.
