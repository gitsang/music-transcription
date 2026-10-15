"""Repair music21's channel reassignment for multi-instrument percussion parts."""
from pathlib import Path
from xml.etree import ElementTree as ET
from .xmlio import load_xml


def fix_drum_channels(path):
    path = Path(path)
    _, root = load_xml(path)
    changed = False
    for instrument in root.findall('.//midi-instrument'):
        if instrument.find('midi-unpitched') is not None:
            channel = instrument.find('midi-channel')
            if channel is None:
                channel = ET.SubElement(instrument, 'midi-channel')
            channel.text = '10'
            changed = True
    if changed:
        path.write_bytes(ET.tostring(root, encoding='utf-8', xml_declaration=True))
    return path
