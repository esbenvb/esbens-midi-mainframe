Esben's MIDI Mainframe (EMM) @VERSION@
=====================================

This folder is the whole system. To install it:

  1. Format an SD card as FAT32 (MBR partition table). Cards over 32 GB often come
     formatted exFAT and the Raspberry Pi bootloader cannot read that; see
     docs/INSTALL.md in the project for how to format any size as FAT32.
  2. Copy the CONTENTS of this folder onto the card (not the folder itself).
  3. Put the card in a Raspberry Pi 3 (B, B+, A+), Zero 2 W, 4, 400, CM4, CM4S, 5,
     500, 500+, CM5 or CM5 Lite, connect a powered USB hub with your MIDI gear, and
     power on.

First boot
----------
With no network configured, EMM opens a WiFi network called "EMM-Setup-...".
Join it with your phone or laptop; a setup page opens by itself (if not, go to
http://emm.local/ or http://10.42.0.1/). Pick your WiFi and enter its password.
Afterwards open http://emm.local/ from any device on that network.

No WiFi? Plug in Ethernet instead and open http://emm.local/.

Settings from a text file
-------------------------
Copy emm.txt.example to emm.txt (same folder), remove the "#" in front of the lines
you want, and reboot. Keys in emm.txt win over the web interface. To get the setup
network back, create an empty file called reset-network next to emm.txt and reboot.

What is on the card
-------------------
  kernel_pi3.img / kernel8.img / kernel_2712.img   Linux for Pi 3 and Zero 2 W / Pi 4 / Pi 5
  rootfs-pi3.cpio.gz / rootfs-pi4.cpio.gz / rootfs-pi5.cpio.gz   the system itself (runs from RAM)
  emmd                            the EMM application. The system starts it from here, so it can be
                                  replaced alone (emmd.previous is kept as the fallback)
  bootcode.bin, start*.elf, fixup*.dat, *.dtb   Raspberry Pi firmware pieces and device trees
  config.txt, cmdline.txt         boot configuration
  emm.txt                         your settings (optional)
  emm/state.json                  routing, presets and names made in the web interface (created by EMM)
  emm/system.json                 network, web password and SSH made in the web interface (secrets).
                                  Not part of a web-interface backup. To prepare a card, put the network
                                  and password in emm.txt instead, or copy this file from another unit
                                  (create the emm folder first).

Do not unplug the power while changing settings in the web interface; at all
other times it is safe to just pull the plug.
