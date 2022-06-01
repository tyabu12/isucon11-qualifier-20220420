fetch:
	git fetch -p
	git reset --hard origin/master
	git clean -df
	cp conf/.gitconfig ~

deploy-app1:
	sudo cp conf/mariadb.conf.d/* /etc/mysql/mariadb.conf.d/
	sudo cp conf/nginx/nginx.conf /etc/nginx/nginx.conf
	sudo cp conf/nginx/sites-enabled/* /etc/nginx/sites-enabled
	$(MAKE) build
	$(MAKE) restart-nginx restart-app restart-db
	$(MAKE) clean-log

deploy-app2:
	sudo systemctl stop nginx mariadb
	$(MAKE) build
	$(MAKE) restart-app
	$(MAKE) clean-log

deploy-app3:
	sudo systemctl stop nginx mariadb
	$(MAKE) build
	$(MAKE) restart-app
	$(MAKE) clean-log

build:
	(cd go; go build -o isucondition main.go)

restart: restart-db restart-nginx

restart-app:
	sudo systemctl restart isucondition.go

restart-db:
	sudo systemctl restart mariadb

restart-nginx:
	sudo systemctl restart nginx

clean-log:
	sudo truncate -s 0 /var/log/nginx/access.log /var/log/nginx/error.log /var/log/mysql/mysql.log /var/log/mysql/mariadb-slow.log

install-alp:
	cd /tmp
	wget https://github.com/tkuchiki/alp/releases/download/v1.0.8/alp_linux_amd64.zip
	unzip alp_linux_amd64.zip
	sudo mv ./alp /usr/local/bin
	rm alp_linux_amd64.zip

publish-mysql-host:
	mysql -uisucon -pisucon mysql -e"RENAME USER 'isucon'@'localhost' to 'isucon'@'%';"

alp-log:
	sudo cat /var/log/nginx/access.log | alp ltsv -m '/api/condition/[0-9a-z\-]+,/isu/[0-9a-z\-]+/icon,/isu/[0-9a-z\-]+/graph,/isu/[0-9a-z\-]+/condition,/api/isu/[0-9a-z\-]+,/isu/[0-9a-z\-]+' -r

recreate-sql-init-data:
	./sql/init.sh
	mysqldump -uisucon -pisucon -t isucondition > ./sql/1_InitData.sql

extract-icon:
	./sql/init.sh
	(cd cmd/extracticon; go build; ./extracticon)