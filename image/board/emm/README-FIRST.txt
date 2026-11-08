Esben's MIDI Mainframe (EMM) @VERSION@
=====================================

This folder is the whole system. To install it:

  1. Format an SD card as FAT32 (MBR partition table). Cards over 32 GB often come
     formatted exFAT and the Raspberry Pi 4/5 bootloader cannot read that; see
     docs/INSTALL.md in the project for how to format any size as FAT32.
  2. Copy the CONTENTS of this folder onto the card (not the folder itself).
  3. Put the card in a Raspberry Pi 4, 400, CM4, CM4S, 5 or CM5 (not the Pi 500/500+
     or the CM5 Lite), connect a powered USB hub with your MIDI gear, and power on.

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
  kernel8.img / kernel_2712.img   Linux for Pi 4 / Pi 5
  rootfs-pi4.cpio.gz / rootfs-pi5.cpio.gz   the system itself (runs from RAM)
  start4.elf, fixup4.dat, *.dtb   Raspberry Pi firmware pieces and device trees
  config.txt, cmdline.txt         boot configuration
  emm.txt                         your settings (optional)
  emm/state.json                  settings made in the web interface (created by EMM)

Do not unplug the power while changing settings in the web interface; at all
other times it is safe to just pull the plug.
