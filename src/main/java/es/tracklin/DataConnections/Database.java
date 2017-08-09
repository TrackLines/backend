package es.tracklin.DataConnections;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.Configuration;
import org.springframework.stereotype.Component;

@Component("DatabaseDataConnection")
@Configuration
public class Database {
    private es.tracklin.Configuration.Database databaseConfiguration;

    @Autowired
    public void setDatabaseConfiguration(es.tracklin.Configuration.Database databaseConfiguration) {
        this.databaseConfiguration = databaseConfiguration;
    }

    public int returnPort() {
        return databaseConfiguration.getPort();
    }
}
