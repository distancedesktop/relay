#!/usr/bin/env python3
"""A UDP delay proxy: adds a fixed one-way delay to every datagram in both
directions so a loopback QUIC path behaves like a WAN path.

  client  ->  udp 127.0.0.1:<listen>  ->  (delay)  ->  <target>
          <-                          <-  (delay)  <-

Single client at a time (last-seen source address wins), which is all the spike
needs. No loss, no reordering beyond what the delay scheduler introduces.

Usage: delayproxy.py <listen_host:port> <target_host:port> <one_way_delay_ms>
"""
import asyncio
import sys


class Proxy(asyncio.DatagramProtocol):
    def __init__(self, target, delay_s):
        self.target = target
        self.delay = delay_s
        self.transport = None
        self.client = None
        self.n_up = 0
        self.n_down = 0

    def connection_made(self, transport):
        self.transport = transport

    def datagram_received(self, data, addr):
        loop = asyncio.get_running_loop()
        if addr == self.target:
            self.n_down += 1
            dst = self.client
        else:
            self.n_up += 1
            self.client = addr
            dst = self.target
        if dst is None:
            return
        loop.call_later(self.delay, self._send, data, dst)

    def _send(self, data, dst):
        try:
            self.transport.sendto(data, dst)
        except Exception:
            pass


async def main():
    listen_h, listen_p = sys.argv[1].rsplit(":", 1)
    tgt_h, tgt_p = sys.argv[2].rsplit(":", 1)
    delay_ms = float(sys.argv[3])
    loop = asyncio.get_running_loop()
    proto = Proxy((tgt_h, int(tgt_p)), delay_ms / 1000.0)
    await loop.create_datagram_endpoint(lambda: proto, local_addr=(listen_h, int(listen_p)))
    print("delayproxy: %s -> %s, %.1f ms each way (%.1f ms RTT added)" %
          (sys.argv[1], sys.argv[2], delay_ms, 2 * delay_ms), flush=True)
    while True:
        await asyncio.sleep(5)
        print("delayproxy: up=%d down=%d" % (proto.n_up, proto.n_down), flush=True)


asyncio.run(main())
