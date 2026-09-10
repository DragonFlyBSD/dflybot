#!/usr/bin/env python3

"""
License: MIT License
Copyright (c) 2023 Miel Donkers
Copyright (c) 2025-2026 Aaron LI

Very simple HTTP server in Python for logging requests.

Derived from: https://gist.github.com/mdonkers/63e115cc0c79b4f6b8b3a6b797e485c7

Changes:
- Added -h | --help
- Added support for PUT and DELETE requests
- Added support of specifying bind address
- Added IPv6 support
"""

import argparse
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, HTTPServer
import logging
import socket

class S(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    error_content_type = 'text/plain'
    error_message_format = 'Error %(code)d: %(message)s'

    def _write_response(self, code=HTTPStatus.OK, content_type='text/plain',
                        content=None):
        if content:
            content = content.encode('utf-8')
        self.send_response(code)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(content or '')))
        self.end_headers()
        if content:
            self.wfile.write(content)

    def _log_request(self, body=None):
        logging.info(f'{self.client_address} {self.command} {self.path} ' +
                     f'{self.request_version}\n' +
                     '------ [headers] ------\n' +
                     f'{self.headers}' +
                     '------ [body] ------\n' +
                     f'{body}\n'
                     '------ [end] ------')

    def do_GET(self):
        self._log_request()
        self._write_response()

    def do_POST(self):
        content_length = int(self.headers.get('Content-Length', 0))
        if content_length > 0:
            body = self.rfile.read(content_length).decode('utf-8')
        else:
            body = None
        self._log_request(body=body)
        self._write_response()

    def do_PUT(self):
        return self.do_POST()

    def do_DELETE(self):
        return self.do_GET()

def run(port=8080, addr='127.0.0.1', *, server_class=HTTPServer, handler_class=S):
    host = addr
    family = socket.AF_INET
    if ':' in addr:
        host = f'[{addr}]'
        family = socket.AF_INET6
    logging.info(f'Starting at http://{host}:{port}/ ...')
    if server_class.address_family != family:
        server_class = type(server_class.__name__, (server_class,),
                            {'address_family': family})
    httpd = server_class((addr, port), handler_class)
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    httpd.server_close()

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='Simple HTTP request dumper')
    parser.add_argument('-b', '--bind', default='127.0.0.1',
                        help='bind address (default: 127.0.0.1)')
    parser.add_argument('port', type=int, nargs='?', default=8080,
                        help='listening port (default: 8080)')
    args = parser.parse_args()

    logging.basicConfig(level=logging.INFO)
    run(port=args.port, addr=args.bind)
