package es.tracklin.DataConnections;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.Configuration;
import org.springframework.stereotype.Component;

@Component("HibernateDataConnection")
@Configuration
public class Hibernate {
    private es.tracklin.Configuration.Hibernate hibernateConfiguration;

    @Autowired
    public void setHibernateConfiguration(es.tracklin.Configuration.Hibernate hibernateConfiguration) {
        this.hibernateConfiguration = hibernateConfiguration;
    }

    public int returnPort() {
        return hibernateConfiguration.getPort();
    }
}
