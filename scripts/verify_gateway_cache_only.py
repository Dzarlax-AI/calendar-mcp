#!/usr/bin/env python3
"""Exercise six strict Calendar reads through a disposable loopback Gateway."""
import argparse
import hashlib
import json
import os
import pathlib
import select
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

from verify_calendar_coverage_gateway import ALLOWED, DENIED, UPSTREAM, result, rpc, tools_at

READS = (
    'list_calendars', 'get_calendar_capabilities', 'get_events',
    'get_event', 'get_event_instances', 'search_events',
)


def response_data(response):
    value = result(response)
    return value.get('structuredContent') or json.loads(value['content'][0]['text'])


def error_code(response):
    assert response.get('result', {}).get('isError'), response
    return (response['result'].get('structuredContent') or {}).get('code')


def fixture_state(control, phase=None):
    request = (urllib.request.Request(f'{control}?phase={phase}', method='POST') if phase else control)
    with urllib.request.urlopen(request, timeout=30 if phase else 5) as response:
        return json.load(response)


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=8)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def verify(info, gateway):
    endpoint = info['endpoint']
    base = {name: info[name] for name in ('start', 'end')}
    affected, healthy = info['affected'], info['healthy']

    def call(tool, arguments, key=ALLOWED):
        response, _ = rpc(gateway, key, 'tools/call', {'name': 'invoke_tool', 'arguments': {
            'server_id': 'calendar', 'tool_name': tool, 'arguments': arguments,
        }})
        return response

    def point(calendar_id):
        return {'calendar_id': calendar_id, 'event_id': 'apple:master'}

    def sources(page):
        return {entry['calendar_id'] for entry in page['sources']}

    def page_items(page):
        return {event['calendar_id'] for event in page['items']}

    strict = tools_at(endpoint)
    modern = tools_at(f'{endpoint}/modern')
    compat = tools_at(f'{endpoint}/compat')
    assert set(READS) <= set(strict)
    for name in READS:
        if name != 'get_events':
            assert strict[name]['inputSchema'] == modern[name]['inputSchema']
            assert strict[name].get('annotations') == modern[name].get('annotations')
    assert strict['get_events']['annotations']['readOnlyHint']
    assert 'calendar_ids' in strict['get_events']['inputSchema']['properties']
    assert 'calendar_id' in modern['get_events']['inputSchema']['properties']
    assert 'get_event_v2' in compat

    def phase_assertions(active):
        before = fixture_state(info['control'])['calls']
        first = call('list_calendars', {})
        if active:
            assert {item['id'] for item in response_data(first)} == {affected, healthy}
        else:
            assert error_code(first) == 'unavailable'
        for calendar_id in (affected, healthy):
            queries = [
                ('get_calendar_capabilities', {'calendar_id': calendar_id}),
                ('get_event', point(calendar_id)),
                ('get_event_instances', {**point(calendar_id), **base, 'show_deleted': True}),
                ('search_events', {**base, 'calendar_id': calendar_id, 'query': 'NEEDLE'}),
            ]
            for tool, args in queries:
                value = call(tool, args)
                if calendar_id == affected and not active:
                    assert error_code(value) == 'unavailable'
                    assert affected not in json.dumps(value)
                    continue
                out = response_data(value)
                if tool == 'get_event_instances':
                    assert {item['id'] for item in out['items']} == {'apple:instance', 'apple:cancelled-instance'}
                elif tool == 'search_events':
                    assert page_items(out) == {calendar_id}
                elif tool == 'get_event':
                    assert out['id'] == 'apple:master'
                else:
                    assert out['calendar_id'] == calendar_id and out['operations']['list']
        expected = {affected, healthy} if active else {healthy}
        for tool, args in [('get_events', base), ('search_events', {**base, 'query': 'needle'})]:
            out = response_data(call(tool, args))
            assert sources(out) == expected and page_items(out) == expected
            assert out['partial'] is (not active)
            assert out['complete'] is active
            assert len(json.dumps(result(call(tool, args)), separators=(',', ':')).encode()) <= 16384
        assert fixture_state(info['control'])['calls'] == before

    phase_assertions(True)
    first_page = response_data(call('get_events', {**base, 'page_size': 1}))
    stale_cursor = first_page['next_page_token']
    fixture_state(info['control'], 'grace')
    phase_assertions(True)
    assert response_data(call('get_events', {**base, 'page_size': 1, 'page_token': stale_cursor}))['complete']
    fixture_state(info['control'], 'expired')
    phase_assertions(False)
    assert call('get_events', {**base, 'page_size': 1, 'page_token': stale_cursor})['result']['isError']
    fixture_state(info['control'], 'renewed')
    phase_assertions(True)
    assert call('get_events', {**base, 'page_size': 1, 'page_token': stale_cursor})['result']['isError']
    assert call('get_event', point(healthy), DENIED)['result']['isError']

    operations = [
        ('list_calendars', {}), ('get_calendar_capabilities', {'calendar_id': healthy}),
        ('get_events', base), ('get_event', point(healthy)),
        ('get_event_instances', {**base, **point(healthy)}),
        ('search_events', {**base, 'query': 'needle'}),
    ]
    before = fixture_state(info['control'])['calls']
    batch, _ = rpc(gateway, ALLOWED, 'tools/call', {'name': 'invoke_read_batch', 'arguments': {
        'operations': [{'id': str(i), 'server_id': 'calendar', 'tool_name': name, 'arguments': args}
                       for i, (name, args) in enumerate(operations)],
    }})
    payload = response_data(batch)
    assert payload['complete'] and len(payload['results']) == len(READS)
    assert all(item['dispatched'] and item['status'] == 'ok' for item in payload['results'])
    assert fixture_state(info['control'])['calls'] == before

    def create(title):
        args = {'calendar_id': affected, 'notification_policy': 'none', 'event': {
            'title': title,
            'start': {'date_time': base['start'], 'time_zone': 'UTC'},
            'end': {'date_time': base['end'], 'time_zone': 'UTC'},
        }}
        return call('create_event', args)

    written = result(create('known response'))
    assert written['structuredContent']['id'] == 'apple:acknowledged'
    assert written['_meta']['calendar/cache_publication'] == 'pending'
    assert written['_meta']['calendar/warnings']
    assert response_data(call('get_event', {'calendar_id': affected, 'event_id': 'apple:acknowledged'}))['id'] == 'apple:acknowledged'
    assert error_code(create('lost response')) == 'mutation_outcome_unknown'
    before = fixture_state(info['control'])
    assert before['writes'] == 2 and before['lookups'] == 0
    after = fixture_state(info['control'], 'recover-write')
    assert after['writes'] == before['writes'] and after['lookups'] == 1
    recovered = response_data(call('get_event', {'calendar_id': affected, 'event_id': 'apple:recovered'}))
    assert recovered['id'] == 'apple:recovered' and recovered['title'] == 'lost response'



def verify_pending_write(info, gateway, operation, applied_write=False):
    affected, destination, healthy = (info[key] for key in ('affected', 'destination', 'healthy'))
    calendars = {affected, destination, healthy, info['sibling']}
    touched = {affected, destination} if operation == 'move_event' else {affected}
    base = {key: info[key] for key in ('start', 'end')}
    control = info['control']

    def call(name, args):
        response, _ = rpc(gateway, ALLOWED, 'tools/call', {'name': 'invoke_tool', 'arguments': {
            'server_id': 'calendar', 'tool_name': name, 'arguments': args,
        }})
        return response

    def check_reads(pending, *, after_timeout=False, fresh=(), hidden=frozenset(), removed=frozenset()):
        before = fixture_state(control)
        listing = call('list_calendars', {})
        if hidden:
            assert error_code(listing) == 'unavailable'
        else:
            assert {calendar['id'] for calendar in response_data(listing)} == calendars
        for calendar_id in calendars - hidden:
            args = {'calendar_id': calendar_id}
            caps = response_data(call('get_calendar_capabilities', args))
            assert caps['calendar_id'] == calendar_id and caps['operations']['get']
            for event_id in ('apple:master', 'apple:instance'):
                point = call('get_event', {**args, 'event_id': event_id})
                if calendar_id in pending:
                    expected = 'unavailable' if after_timeout else 'cache_pending'
                    assert error_code(point) == expected, point
                elif calendar_id in removed and event_id == 'apple:instance':
                    assert error_code(point) == 'cache_miss', point
                else:
                    event = response_data(point)
                    assert event['id'] == event_id
                    assert event['title'].endswith(' fresh') is (calendar_id in fresh), event
            for tool, query, scope in (
                ('get_event_instances', {**args, 'event_id': 'apple:master'}, {calendar_id}),
                ('search_events', {**args, 'query': 'needle'}, {calendar_id}),
                ('get_events', {'calendar_ids': [calendar_id]}, {calendar_id}),
            ):
                check_page(call(tool, {**base, **query}), pending, scope, after_timeout, removed=removed)
        for calendar_id in hidden:
            assert error_code(call('get_event', {'calendar_id': calendar_id, 'event_id': 'apple:instance'})) == 'unavailable'
        for tool, query in (
            ('get_events', base),
            ('search_events', {**base, 'query': 'needle'}),
        ):
            check_page(call(tool, query), pending, calendars - hidden, after_timeout, bool(hidden), removed)
        assert fixture_state(control) == before

    def check_page(response, pending, scope, after_timeout, missing=False, removed=frozenset()):
        out = response_data(response)
        partial = missing or bool(pending & scope)
        assert out['partial'] is partial and out['complete'] is (not partial), out
        assert {item['calendar_id'] for item in out['items']} == scope - pending - removed, out
        sources = {source['calendar_id']: source for source in out['sources']}
        assert set(sources) == scope, out
        for calendar_id, source in sources.items():
            if calendar_id in pending:
                assert source['status'] == 'pending' and source['coverage'] == 'unavailable', source
                assert source.get('error_code') == (None if after_timeout else 'cache_pending'), source
            else:
                assert source['status'] == 'ready' and source['coverage'] == 'covered', source

    check_reads(set())
    old_page = response_data(call('get_events', {**base, 'page_size': 1}))
    args = {'calendar_id': affected, 'event_id': 'apple:instance', 'notification_policy': 'none'}
    if operation == 'move_event':
        args['destination_calendar_id'] = destination
    else:
        args['scope'] = 'single'
    assert error_code(call(operation, args)) == 'mutation_outcome_unknown'
    assert call('get_events', {**base, 'page_size': 1, 'page_token': old_page['next_page_token']})['result']['isError']

    for phase, blocked, expired, fresh in (
        (None, touched, False, ()),
        ('before-timeout', touched, False, ()),
        ('after-timeout', touched, True, ()),
    ):
        state = fixture_state(control, phase)
        assert state['writes'] == 1 and state['lookups'] == 0, state
        check_reads(blocked, after_timeout=expired, fresh=fresh)
        if phase in ('before-timeout', 'after-timeout'):
            assert state['busy'], state

    for phase in ('offline-1h', 'offline-24h'):
        state = fixture_state(control, phase)
        assert state['writes'] == 1 and state['lookups'] == 0, state
        failing = {info[name] for name, count in state['failures'].items() if count >= 2}
        assert failing == touched, state
        check_reads(set(), after_timeout=True, hidden=touched)
    attempts = state['failures']
    assert all(attempts[name] > 100 for name in ('affected', 'destination') if info[name] in touched), state

    state = fixture_state(control, 'account-offline')
    assert state['writes'] == 1 and state['lookups'] == 0, state
    check_reads(set(), hidden=calendars - {healthy})

    recovered = fixture_state(control, 'resynced')
    assert recovered['writes'] == 1 and recovered['lookups'] == 0, recovered
    assert recovered['failures'] == state['failures']
    removed = {affected} if applied_write else set()
    check_reads(set(), after_timeout=True, fresh=touched, removed=removed)
    if applied_write and operation == 'move_event':
        moved = response_data(call('get_event', {'calendar_id': destination, 'event_id': 'apple:moved-instance'}))
        assert moved['calendar_id'] == destination and moved['title'] == 'needle affected fresh', moved
    assert call('get_events', {**base, 'page_size': 1, 'page_token': old_page['next_page_token']})['result']['isError']


def verify_graph_recovery(info, gateway):
    graph_sources = {info['affected'], info['destination']}
    neighbors = {info['sibling'], info['healthy']}
    control = info['control']
    base = {name: info[name] for name in ('start', 'end')}

    def call(name, arguments):
        response, _ = rpc(gateway, ALLOWED, 'tools/call', {'name': 'invoke_tool', 'arguments': {
            'server_id': 'calendar', 'tool_name': name, 'arguments': arguments,
        }})
        return response

    def check(blocked, fresh):
        before = fixture_state(control)
        for calendar_id in graph_sources | neighbors:
            prefix = calendar_id.partition(':')[0]
            point = call('get_event', {'calendar_id': calendar_id, 'event_id': f'{prefix}:instance'})
            if calendar_id in blocked:
                assert error_code(point) == 'unavailable'
            else:
                event = response_data(point)
                assert event['calendar_id'] == calendar_id
                assert event['title'].endswith(' fresh') is (calendar_id in fresh), event
        page = response_data(call('get_events', base))
        assert {item['calendar_id'] for item in page['items']} == (graph_sources | neighbors) - blocked, page
        assert fixture_state(control) == before

    check(set(), set())
    denied = fixture_state(control, 'graph-denied')
    assert denied['writes'] == 0 and denied['lookups'] == 0
    assert denied['failures']['affected'] == denied['failures']['destination'] == 1
    check(graph_sources, set())
    recovered = fixture_state(control, 'graph-recovered')
    assert recovered['writes'] == 0 and recovered['lookups'] == 0
    assert recovered['failures'] == denied['failures']
    check(set(), graph_sources)


def verify_authority_adapters(info, gateway):
    control = info['control']
    base = {name: info[name] for name in ('start', 'end')}
    family, sibling, blocked, graph = (info[name] for name in ('family', 'sibling', 'affected', 'graph_private'))

    def call(name, arguments):
        response, _ = rpc(gateway, ALLOWED, 'tools/call', {'name': 'invoke_tool', 'arguments': {
            'server_id': 'calendar', 'tool_name': name, 'arguments': arguments,
        }})
        return response

    def point(source, event=None):
        prefix = source.partition(':')[0]
        return call('get_event', {'calendar_id': source, 'event_id': event or f'{prefix}:instance'})

    def check_family_and_sibling():
        before = fixture_state(control)
        out = response_data(point(family, 'apple:/home/family/event.ics'))
        assert out['title'] == 'Real fallback fixture' and out['calendar_id'] == family, out
        assert response_data(point(sibling))['calendar_id'] == sibling
        out = response_data(call('get_events', base))
        assert {event['calendar_id'] for event in out['items']} >= {family, sibling, info['healthy']}, out
        assert fixture_state(control) == before

    assert response_data(point(graph))['visibility'] == 'private'
    assert response_data(point(blocked))['calendar_id'] == blocked
    known_search = response_data(call('search_events', {**base, 'query': 'needle', 'calendar_id': info['healthy']}))
    assert known_search['partial'] and not known_search['complete']
    assert [event['title'] for event in known_search['items']] == ['needle healthy'], known_search
    assert known_search['sources'][0]['error_code'] == 'result_too_large', known_search
    first = fixture_state(control, 'apple-fallback')
    assert first['adapter_calls'][0] >= 2 and first['adapter_calls'][2] == 0, first
    check_family_and_sibling()
    assert response_data(point(blocked))['calendar_id'] == blocked
    assert error_code(call('list_calendars', {})) == 'unavailable'

    expired = fixture_state(control, 'apple-expired')
    assert expired['adapter_calls'][0] > first['adapter_calls'][0], expired
    check_family_and_sibling()
    assert error_code(point(blocked)) == 'unavailable'
    page = response_data(call('get_events', base))
    assert blocked not in {source['calendar_id'] for source in page['sources']} and page['partial']
    assert response_data(point(graph))['visibility'] == 'private'

    denied = fixture_state(control, 'graph-page-error')
    assert denied['adapter_calls'][1:] == [2, 0] and denied['writes'] == 0 and denied['lookups'] == 0, denied
    assert error_code(point(graph)) == 'unavailable'
    out = response_data(call('search_events', {**base, 'query': 'needle graph-private'}))
    assert out['items'] == []
    check_family_and_sibling()
    assert fixture_state(control) == denied


def go_json(value, *, sort_keys=False):
    # Gateway's contract fingerprint uses canonical Go JSON; its default
    # encoder escapes HTML-sensitive characters, including on strings.
    text = json.dumps(value, sort_keys=sort_keys, separators=(',', ':'), ensure_ascii=False)
    for char in ('<', '>', '&', chr(0x2028), chr(0x2029)):
        text = text.replace(char, r'\u%04x' % ord(char))
    return text.encode()


def classifications(endpoint, baseline):
    main, previous = tools_at(endpoint), tools_at(baseline)
    binding = hashlib.sha256(go_json(dict(
        ID='calendar', URL=endpoint, Header='', Source=chr(0) + 'FIXTURE_UPSTREAM',
        Token=UPSTREAM, Allowlist=None,
    ))).hexdigest()
    for name in READS:
        for field in ('inputSchema', 'outputSchema'):
            assert main[name].get(field) == previous[name].get(field), (name, field)
        assert main[name].get('annotations', {}).get('readOnlyHint') == previous[name].get('annotations', {}).get('readOnlyHint'), name
    definitions = {name: previous[name] for name in READS}
    return {name: {'enabled': True, 'access_mode': 'shared', 'operation_kind': 'read',
                   'operation_contract_hash': hashlib.sha256(go_json({
                       'name': name, 'inputSchema': definition['inputSchema'],
                       'outputSchema': definition.get('outputSchema'),
                       'readOnlyHint': True if definition.get('annotations', {}).get('readOnlyHint') else None,
                       'connection': binding,
                   }, sort_keys=True)).hexdigest()} for name, definition in definitions.items()}


def main():
    if sys.flags.optimize:
        raise SystemExit('Gateway verification requires Python assertions.')
    parser = argparse.ArgumentParser()
    parser.add_argument('--fixture', required=True)
    parser.add_argument('--gateway', required=True)
    scenario = parser.add_mutually_exclusive_group()
    scenario.add_argument('--pending-write', choices=('delete_event', 'move_event'))
    scenario.add_argument('--graph-recovery', action='store_true')
    scenario.add_argument('--authority-adapters', action='store_true')
    parser.add_argument('--applied-write', action='store_true')
    args = parser.parse_args()
    if args.applied_write and not args.pending_write:
        parser.error('--applied-write requires --pending-write')
    with tempfile.TemporaryDirectory(prefix='strict-gateway-verification-') as temporary:
        root = pathlib.Path(temporary)
        with (root/'fixture.log').open('w+') as fixture_log, (root/'gateway.log').open('w+') as gateway_log:
            name = args.pending_write
            if args.authority_adapters:
                name = 'authority-adapters'
            elif args.graph_recovery:
                name = 'graph-recovery'
            fixture_args = [args.fixture, name] if name else [args.fixture]
            if args.applied_write:
                fixture_args.append('--applied-write')
            fixture = subprocess.Popen(fixture_args, stdout=subprocess.PIPE, stderr=fixture_log)
            gateway = None
            try:
                assert select.select([fixture.stdout], [], [], 15)[0], 'Calendar fixture did not start'
                info = json.loads(fixture.stdout.readline(65537))
                with socket.socket() as sock:
                    sock.bind(('127.0.0.1', 0))
                    port = sock.getsockname()[1]
                writes = [] if args.graph_recovery or args.authority_adapters else [args.pending_write or 'create_event']
                write_tools = {name: {'enabled': True, 'access_mode': 'shared', 'operation_kind': 'write'} for name in writes}
                registry = {'registry_version': 2, 'servers': {
                    'calendar': {'url': info['endpoint'], 'token_env': 'FIXTURE_UPSTREAM', 'enabled': True,
                                 'tools': {**classifications(info['endpoint'], info['pre_cutover']), **write_tools}},
                }, 'principals': {
                    'allowed': {'token_env': 'FIXTURE_ALLOWED', 'access_policy': 'restricted',
                                'tool_allowlist': {'calendar': [*READS, *writes]}, 'grants': {}},
                    'denied': {'token_env': 'FIXTURE_DENIED', 'access_policy': 'restricted',
                               'tool_allowlist': {}, 'grants': {}},
                }}
                config = root/'registry.json'
                config.write_text(json.dumps(registry))
                env = {key: value for key, value in os.environ.items() if not key.startswith('GATEWAY_')}
                env.update(FIXTURE_UPSTREAM=UPSTREAM, FIXTURE_ALLOWED=ALLOWED, FIXTURE_DENIED=DENIED,
                           GATEWAY_STORAGE='file', GATEWAY_CONFIG=str(config), GATEWAY_LISTEN_ADDR=f'127.0.0.1:{port}',
                           GATEWAY_TOOL_SEARCH_ENABLED='true', GATEWAY_READ_BATCH_ENABLED='true',
                           GATEWAY_READ_BATCH_POLICY_MODE="classified")
                gateway = subprocess.Popen([args.gateway], env=env, stdout=subprocess.DEVNULL, stderr=gateway_log)
                url = f'http://127.0.0.1:{port}'
                for _ in range(100):
                    assert gateway.poll() is None, 'Gateway exited'
                    try:
                        with urllib.request.urlopen(f'{url}/healthz', timeout=1) as response:
                            if response.status == 200:
                                break
                    except OSError:
                        time.sleep(.05)
                else:
                    raise RuntimeError('Gateway did not become healthy')
                if args.pending_write:
                    verify_pending_write(info, f'{url}/mcp', args.pending_write, args.applied_write)
                elif args.graph_recovery:
                    verify_graph_recovery(info, f'{url}/mcp')
                elif args.authority_adapters:
                    verify_authority_adapters(info, f'{url}/mcp')
                else:
                    verify(info, f'{url}/mcp')
            except Exception:
                for log in (fixture_log, gateway_log):
                    log.seek(0)
                    print(log.read(4000))
                raise
            finally:
                if gateway:
                    stop(gateway)
                stop(fixture)
    if args.authority_adapters:
        print('Verified Family Sharing ICS fallback, CalDAV 207 sibling isolation, and immediate Graph private revocation before the next page failed. Zero writes.')
        return
    if args.graph_recovery:
        print('Verified Graph recovery from delta 401 and 403 after positive authority, with uninterrupted neighboring reads and zero writes.')
        return
    if args.pending_write:
        print(f'Verified {args.pending_write} (applied={args.applied_write}): expiration while the normal worker was blocked, 24-hour source isolation, and scheduled read-only recovery. One synthetic write, zero repeats.')
        return
    print('Verified six reads, grace, source expiry and recovery, cursor fencing, batch, ACL, write warnings and lost receipt recovery. Zero provider calls during reads and zero repeat dispatches.')


if __name__ == '__main__':
    main()
