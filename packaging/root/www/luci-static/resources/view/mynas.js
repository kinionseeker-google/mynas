'use strict';
'require view';
'require form';
'require fs';
'require uci';
'require rpc';

var callServiceList = rpc.declare({
	object: 'service',
	method: 'list',
	params: [ 'name' ],
	expect: { '': {} }
});

function isRunning(res) {
	try {
		var inst = res.mynas.instances;
		for (var k in inst)
			if (inst[k].running)
				return true;
	} catch (e) {}
	return false;
}

return view.extend({
	load: function() {
		return Promise.all([
			uci.load('mynas'),
			L.resolveDefault(callServiceList('mynas'), {}),
			L.resolveDefault(fs.trimmed('/var/run/mynas.state'), '')
		]);
	},

	renderStatus: function(running, state) {
		var port = uci.get('mynas', 'main', 'port') || '8580';
		var url = 'http://' + window.location.hostname + ':' + port + '/';
		var body;

		if (running && state == 'waiting') {
			body = [
				E('strong', {}, _('等待磁盘挂载')),
				E('span', {}, ' — ' + _('文件目录所在的磁盘还没有挂载，挂载后会自动启动。'))
			];
		} else if (running) {
			body = [
				E('strong', { 'style': 'color:#2e7d32' }, _('运行中')),
				E('span', {}, ' '),
				E('a', { 'class': 'btn cbi-button cbi-button-action', 'href': url, 'target': '_blank', 'rel': 'noopener' }, _('打开管理界面')),
				E('span', { 'style': 'margin-left:.8em' }, url)
			];
		} else {
			body = [ E('strong', {}, _('未运行')) ];
		}

		return E('div', { 'class': 'cbi-section' }, [
			E('h3', {}, _('运行状态')),
			E('p', { 'id': 'mynas-status' }, body)
		]);
	},

	render: function(data) {
		var status = this.renderStatus(isRunning(data[1]), data[2]);
		var m, s, o;

		m = new form.Map('mynas', _('黑曜石云盘'),
			_('网盘式的私有 NAS：文件管理、外部链接、离线下载和外部网盘。首次打开管理界面时创建管理员账号。'));

		s = m.section(form.NamedSection, 'main', 'mynas', _('设置'));
		s.addremove = false;

		o = s.option(form.Flag, 'enabled', _('启用'));
		o.rmempty = false;

		o = s.option(form.Value, 'port', _('管理界面端口'));
		o.datatype = 'port';
		o.placeholder = '8580';
		o.rmempty = false;

		o = s.option(form.Value, 'files_dir', _('文件目录'),
			_('管理界面里“全部文件”对应的目录。填硬盘的挂载点（例如 /mnt/sda1）可以直接看到硬盘上已有的文件；也可以填其中一个子目录，只管理那个目录。'));
		o.rmempty = false;
		o.validate = function(section_id, value) {
			return (value && value.charAt(0) == '/') ? true : _('请填写以 / 开头的完整路径');
		};

		o = s.option(form.Flag, 'require_mount', _('等磁盘挂载后再启动'),
			_('文件目录在外接硬盘上时请开启：硬盘没挂载时不启动，避免把文件写进路由器闪存。'));
		o.rmempty = false;

		o = s.option(form.Value, 'min_free', _('保留空间（MB）'),
			_('文件目录所在磁盘的剩余空间低于这个值时，拒绝上传和下载。'));
		o.datatype = 'uinteger';
		o.placeholder = '64';

		o = s.option(form.Value, 'data_dir', _('配置目录'),
			_('保存账号、链接、任务记录等配置，体积很小。'));
		o.placeholder = '/etc/mynas';
		o.validate = function(section_id, value) {
			return (!value || value.charAt(0) == '/') ? true : _('请填写以 / 开头的完整路径');
		};

		o = s.option(form.Value, 'proxy_ports', _('代理内嵌端口范围'),
			_('外部链接开启“代理内嵌”时使用，每个链接占用一个端口。'));
		o.placeholder = '18600-18649';
		o.validate = function(section_id, value) {
			return (!value || /^\d{1,5}-\d{1,5}$/.test(value)) ? true : _('格式为 起始端口-结束端口，例如 18600-18649');
		};

		// Show the status between the page title and the settings.
		return m.render().then(function(node) {
			var first = node.querySelector('.cbi-section');
			if (first && first.parentNode)
				first.parentNode.insertBefore(status, first);
			else
				node.appendChild(status);
			return node;
		});
	}
});
