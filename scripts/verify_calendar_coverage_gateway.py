#!/usr/bin/env python3
"""Exercise local Calendar and Gateway binaries with disposable synthetic data."""
import argparse
import json
import os
import pathlib
import socket
import statistics
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

UPSTREAM = 'synthetic-upstream-key'
ALLOWED = 'synthetic-allowed-key-123456'
DENIED = 'synthetic-denied-key-123456'
SESSIONS = {}


def rpc(url, token, method, params):
    identity = (url, token)
    if identity not in SESSIONS and method != 'initialize':
        rpc(url, token, 'initialize', {
            'protocolVersion': '2025-03-26', 'capabilities': {},
            'clientInfo': {'name': 'fixture', 'version': '1'},
        })
    body = json.dumps({'jsonrpc': '2.0', 'id': 1, 'method': method, 'params': params}).encode()
    headers = {
        'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json',
        'Accept': 'application/json, text/event-stream', 'MCP-Protocol-Version': '2025-03-26',
    }
    if SESSIONS.get(identity):
        headers['Mcp-Session-Id'] = SESSIONS[identity]
    request = urllib.request.Request(url, data=body, headers=headers)
    with urllib.request.urlopen(request, timeout=10) as response:
        raw = response.read()
        if method == 'initialize':
            SESSIONS[identity] = response.headers.get('Mcp-Session-Id', '')
    if raw.startswith(b'data: '):
        raw = raw.split(b'data: ', 1)[1].split(b'\n', 1)[0]
    return json.loads(raw), len(raw)


def result(value):
    if 'error' in value or value['result'].get('isError'):
        raise RuntimeError(str(value))
    return value['result']


def tools_at(endpoint):
    response, _ = rpc(endpoint, UPSTREAM, 'tools/list', {})
    return {tool['name']: tool for tool in result(response)['tools']}


def walk(mode, info, gateway_url, all_ready):
    token = ''
    ids, sizes = [], []
    started = time.monotonic()
    first_ms = None
    while True:
        arguments = {'start': info['start'], 'end': info['end'], 'page_size': 5}
        if token:
            arguments['page_token'] = token
        operation = {'server_id': 'calendar', 'tool_name': 'get_events', 'arguments': arguments}
        if mode == 'batch':
            endpoint, key = gateway_url, ALLOWED
            call = {'name': 'invoke_read_batch', 'arguments': {'operations': [{'id': 'one', **operation}]}}
        elif mode == 'gateway':
            endpoint, key = gateway_url, ALLOWED
            call = {'name': 'invoke_tool', 'arguments': operation}
        else:
            endpoint, key = info['endpoint'], UPSTREAM
            call = {'name': 'get_events', 'arguments': arguments}
        response, size = rpc(endpoint, key, 'tools/call', call)
        tool_result = result(response)
        sizes.append(size)
        if first_ms is None:
            first_ms = (time.monotonic() - started) * 1000
        assert 'result_omitted' not in json.dumps(tool_result)
        if mode == 'batch':
            batch = tool_result['structuredContent']
            assert batch['complete'] and len(batch['results']) == 1
            entry = batch['results'][0]
            assert entry['status'] == 'ok' and entry['dispatched'] and not entry.get('upstream_is_error')
            tool_result = entry['tool_result']
        assert len(json.dumps(tool_result, separators=(',', ':')).encode()) <= 16384
        page = tool_result.get('structuredContent') or json.loads(tool_result['content'][0]['text'])
        assert 'synthetic-description-' not in json.dumps(page)
        assert 'fixture-meeting' not in json.dumps(page)
        ids.extend(item['event_id'] for item in page['items'])
        token = page.get('next_page_token', '')
        if not token:
            break
        assert len(sizes) < 30
    expected = {
        f'{provider}:event-{provider}-{provider}-fixture-cal-{index}-{event}'
        for provider, count in [('google', 5), ('apple', 3 if all_ready else 2)]
        for index in range(count) for event in range(4)
    }
    assert len(ids) == len(expected) and set(ids) == expected
    assert len(page['sources']) == 8
    assert page['partial'] is not all_ready and page['complete'] is all_ready
    assert sum(source['coverage'] == 'covered' for source in page['sources']) == (8 if all_ready else 7)
    if not all_ready:
        assert any(source['calendar_id'] == 'apple:apple-fixture:cal-2'
                   and source['status'] == 'parked' and source['error_code'] == 'permission'
                   for source in page['sources'])
    return {
        'rounds': len(sizes), 'first_ms': first_ms,
        'walk_ms': (time.monotonic() - started) * 1000,
        'response_bytes': sum(sizes), 'max_response_bytes': max(sizes), 'ids': ids,
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--fixture', default='/tmp/calendar-coverage-fixture')
    parser.add_argument('--gateway', default='/tmp/mcp-gateway')
    parser.add_argument('--warm-walks', type=int, default=5)
    parser.add_argument('--all-ready', action='store_true')
    args = parser.parse_args()
    assert 1 <= args.warm_walks <= 100
    processes = []
    try:
        with tempfile.TemporaryDirectory(prefix='calendar-coverage-gateway-') as temporary:
            fixture_command = [args.fixture]
            if args.all_ready:
                fixture_command.append('-all-ready')
            fixture = subprocess.Popen(fixture_command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            processes.append(fixture)
            line = fixture.stdout.readline()
            if not line:
                raise RuntimeError('fixture failed: ' + fixture.stderr.read())
            info = json.loads(line)
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0))
                port = sock.getsockname()[1]
            tools = {'get_events': {'enabled': True, 'access_mode': 'shared'},
                     'get_event': {'enabled': True, 'access_mode': 'shared'}}
            registry = {
                'servers': {'calendar': {'url': info['endpoint'], 'token_env': 'FIXTURE_UPSTREAM',
                                         'enabled': True, 'tools': tools},
                            'legacy': {'url': info['endpoint'] + '/modern',
                                       'token_env': 'FIXTURE_UPSTREAM', 'enabled': True,
                                       'tools': {'get_events': {'enabled': True, 'access_mode': 'shared'}}}},
                'principals': {
                    'allowed': {'token_env': 'FIXTURE_ALLOWED', 'access_policy': 'restricted',
                                'tool_allowlist': {'calendar': list(tools), 'legacy': ['get_events']}, 'grants': {}},
                    'denied': {'token_env': 'FIXTURE_DENIED', 'access_policy': 'restricted',
                               'tool_allowlist': {}, 'grants': {}},
                }, 'registry_version': 2,
            }
            path = pathlib.Path(temporary) / 'registry.json'
            path.write_text(json.dumps(registry))
            policy = pathlib.Path(temporary) / 'policy.json'
            policy.write_text(json.dumps({'version': 1, 'operations': [
                {'server_id': 'calendar', 'tool_name': 'get_events', 'endpoint': info['endpoint']},
                {'server_id': 'legacy', 'tool_name': 'get_events',
                 'endpoint': info['endpoint'] + '/modern'},
            ]}))
            env = {k: v for k, v in os.environ.items() if not k.startswith('GATEWAY_')}
            env.update({
                'FIXTURE_UPSTREAM': UPSTREAM, 'FIXTURE_ALLOWED': ALLOWED, 'FIXTURE_DENIED': DENIED,
                'GATEWAY_STORAGE': 'file', 'GATEWAY_CONFIG': str(path),
                'GATEWAY_LISTEN_ADDR': f'127.0.0.1:{port}', 'GATEWAY_TOOL_SEARCH_ENABLED': 'true',
                'GATEWAY_READ_BATCH_ENABLED': 'true', 'GATEWAY_READ_BATCH_ALLOWLIST': str(policy),
            })
            gateway = subprocess.Popen([args.gateway], env=env, stdout=subprocess.DEVNULL,
                                       stderr=subprocess.PIPE, text=True)
            processes.append(gateway)
            base = f'http://127.0.0.1:{port}'
            for _ in range(100):
                try:
                    with urllib.request.urlopen(base + '/healthz', timeout=1) as response:
                        if response.status == 200:
                            break
                except OSError:
                    time.sleep(.05)
            else:
                raise RuntimeError('gateway unavailable: ' + gateway.stderr.read())

            catalog = tools_at(info['endpoint'])
            assert 'get_event' in catalog and 'get_events' in catalog and 'get_cached_events' not in catalog
            annotations = catalog['get_events']['annotations']
            assert annotations['readOnlyHint'] and not annotations['destructiveHint'] and not annotations['openWorldHint']
            root = info['endpoint']
            modern, compat = tools_at(root + '/modern'), tools_at(root + '/compat')
            for suffix in ('/modern-v2', '/modern-v2/'):
                request = urllib.request.Request(root + suffix, headers={'Authorization': 'Bearer ' + UPSTREAM})
                try:
                    with urllib.request.urlopen(request, timeout=5):
                        raise AssertionError('removed route responded successfully')
                except urllib.error.HTTPError as error:
                    assert error.code == 404
                    error.close()
            new_schema = catalog['get_events']['inputSchema']
            assert modern['get_events']['inputSchema'] != new_schema
            assert compat['get_events']['inputSchema'] != new_schema
            assert modern['get_events']['inputSchema'] != compat['get_events']['inputSchema']
            cold_start = time.monotonic()
            search_response, _ = rpc(base + '/mcp', ALLOWED, 'tools/call',
                                     {'name': 'search_tools', 'arguments': {'query': 'get_events'}})
            cold_search_ms = (time.monotonic() - cold_start) * 1000
            discovered = result(search_response)['structuredContent']['tools']
            item = next(item for item in discovered if item['tool_name'] == 'get_events')
            assert item['tool_definition']['annotations']['readOnlyHint'] is True
            assert item['batch_read_allowed'] is True
            classification = {key: item.get(key) for key in ('operation_kind', 'operation_source', 'operation_reason')}
            assert classification['operation_kind'] == 'read'
            measurements = {mode: [] for mode in ('direct', 'gateway', 'batch')}
            reference = None
            for _ in range(args.warm_walks + 1):
                for mode in measurements:
                    sample = walk(mode, info, base + '/mcp', args.all_ready)
                    if reference is None:
                        reference = sample['ids']
                    assert sample.pop('ids') == reference
                    measurements[mode].append(sample)
            detail_args = {'calendar_id': 'google:google-fixture:cal-0',
                           'event_id': 'google:event-google-google-fixture-cal-0-0'}
            detail_response, _ = rpc(info['endpoint'], UPSTREAM, 'tools/call',
                                     {'name': 'get_event', 'arguments': detail_args})
            detail_result = result(detail_response)
            detail = detail_result.get('structuredContent') or json.loads(detail_result['content'][0]['text'])
            assert detail['description'].startswith('synthetic-description-')
            assert detail['conference']['entry_points'][0]['uri'] == 'https://example.invalid/fixture-meeting'
            gateway_detail_response, _ = rpc(base + '/mcp', ALLOWED, 'tools/call', {
                'name': 'invoke_tool', 'arguments': {'server_id': 'calendar', 'tool_name': 'get_event',
                                                       'arguments': detail_args},
            })
            gateway_detail = result(gateway_detail_response)
            assert gateway_detail.get('structuredContent') == detail
            live_args = {'start': info['start'], 'end': info['end']}
            live_started = time.monotonic()
            live_response, live_bytes = rpc(root + '/modern', UPSTREAM, 'tools/call',
                                            {'name': 'get_events', 'arguments': live_args})
            live_ms = (time.monotonic() - live_started) * 1000
            live_result = result(live_response)
            live_page = live_result.get('structuredContent') or json.loads(live_result['content'][0]['text'])
            assert len(live_page['items']) == len(reference) and {x['id'] for x in live_page['items']} == set(reference)
            assert live_bytes > 65536
            batch_started = time.monotonic()
            legacy_batch, legacy_batch_bytes = rpc(base + '/mcp', ALLOWED, 'tools/call', {
                'name': 'invoke_read_batch', 'arguments': {'operations': [{
                    'id': 'legacy', 'server_id': 'legacy', 'tool_name': 'get_events', 'arguments': live_args,
                }]},
            })
            legacy_batch_ms = (time.monotonic() - batch_started) * 1000
            legacy_result = result(legacy_batch)['structuredContent']
            assert legacy_result['results'][0]['status'] == 'result_omitted'
            assert legacy_result['results'][0]['dispatched'] is True
            denied, _ = rpc(base + '/mcp', DENIED, 'tools/call', {
                'name': 'invoke_tool', 'arguments': {'server_id': 'calendar', 'tool_name': 'get_events',
                    'arguments': {'start': info['start'], 'end': info['end']}},
            })
            assert 'error' in denied or denied['result'].get('isError')
            report = {'synthetic': True, 'cold_search_ms': cold_search_ms, 'classification': classification,
                      'coverage': '8/8' if args.all_ready else '7/8', 'events': len(reference), 'denied': True, 'separate_catalog_schemas': True,
                      'detail_verified': True, 'live_direct_ms': live_ms, 'live_direct_frame_bytes': live_bytes,
                      'live_batch_frame_bytes': legacy_batch_bytes, 'live_batch_ms': legacy_batch_ms,
                      'live_batch_status': 'result_omitted'}
            for mode, samples in measurements.items():
                warm = samples[1:]
                report[mode] = {'warm_samples': len(warm), 'first_p50_ms': statistics.median(x['first_ms'] for x in warm),
                                'walk_p50_ms': statistics.median(x['walk_ms'] for x in warm),
                                'rounds': warm[0]['rounds'], 'response_bytes': warm[0]['response_bytes'],
                                'max_response_bytes': max(x['max_response_bytes'] for x in warm)}
            print(json.dumps(report))
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=10)


if __name__ == '__main__':
    main()
