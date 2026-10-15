from pathlib import Path, PurePosixPath
import stat
import zipfile
from defusedxml import ElementTree as ET
from .common import PipelineError

MAX_XML_BYTES = 32 * 1024 * 1024


def parse_xml(data):
    if len(data) > MAX_XML_BYTES:
        raise PipelineError('INVALID_MUSICXML', 'MusicXML 解压后过大')
    try:
        # Ordinary MusicXML DOCTYPE is legal; entities and external resolution are not.
        return ET.fromstring(data, forbid_dtd=False, forbid_entities=True, forbid_external=True)
    except Exception as exc:
        raise PipelineError('INVALID_MUSICXML', 'MusicXML 无效或包含不安全实体') from exc


def load_xml(path: Path):
    if not zipfile.is_zipfile(path):
        if path.stat().st_size > MAX_XML_BYTES:
            raise PipelineError('INVALID_MUSICXML', 'MusicXML 文件超过安全解析大小限制')
        return validate(path.read_bytes())
    try:
        with zipfile.ZipFile(path) as archive:
            entries = archive.infolist()
            if len(entries) > 128 or sum(i.file_size for i in entries) > MAX_XML_BYTES:
                raise ValueError('archive limit')
            for entry in entries:
                name = PurePosixPath(entry.filename)
                mode = entry.external_attr >> 16
                if (name.is_absolute() or '..' in name.parts or '\\' in entry.filename
                        or ':' in entry.filename or stat.S_ISLNK(mode) or entry.flag_bits & 1
                        or entry.file_size > MAX_XML_BYTES
                        or entry.file_size / max(1, entry.compress_size) > 1000):
                    raise ValueError('unsafe archive')
            container = parse_xml(archive.read('META-INF/container.xml'))
            roots = [x.get('full-path') for x in container.iter() if x.tag.split('}')[-1] == 'rootfile']
            if not roots or roots[0] not in archive.namelist():
                raise ValueError('missing root')
            return validate(archive.read(roots[0]))
    except PipelineError:
        raise
    except Exception as exc:
        raise PipelineError('INVALID_MUSICXML', '压缩 MusicXML 无效或不安全') from exc


def validate(data):
    root = parse_xml(data)
    if root.tag != 'score-partwise' or not root.findall('part') or root.find('part-list') is None:
        raise PipelineError('INVALID_MUSICXML', '仅支持包含声部的 score-partwise MusicXML')
    for element in root.iter():
        # Renderers must not resolve user-controlled files/URLs (images, opus, links).
        if any(key.split('}')[-1] in ('source', 'href') for key in element.attrib):
            raise PipelineError('INVALID_MUSICXML', '此版本不支持包含外部文件或链接资源的 MusicXML')
    # music21 parses only this safe serialized document, never the uploaded archive.
    from xml.etree.ElementTree import tostring
    return tostring(root, encoding='utf-8', xml_declaration=True), root
